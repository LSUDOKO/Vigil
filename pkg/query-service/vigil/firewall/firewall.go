package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SigNoz/signoz/pkg/query-service/vigil"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/audit"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/engine"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/llm"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/policy"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/recovery"
	"github.com/SigNoz/signoz/pkg/query-service/vigil/telemetry"
)

// Decision is the firewall's answer for one tool call.
type Decision string

const (
	Allow    Decision = "ALLOW"
	Pause    Decision = "PAUSE"
	Block    Decision = "BLOCK"
	Fallback Decision = "FALLBACK"
)

// Stage names which check produced the decision, so the dashboard can show
// *why* rather than just *what*.
const (
	StageIntent   = "intent"
	StageForecast = "forecast"
	StageBehavior = "behavior"
	StageJudge    = "judge"
	StageDefault  = "default"
)

// Call is one tool call awaiting a decision.
type Call struct {
	SessionID   string
	Tool        string
	Args        map[string]any
	ToolCost    float64
	SessionCost float64
	Budget      float64
}

// Result is a decision plus everything needed to explain and audit it.
type Result struct {
	Decision  Decision `json:"decision"`
	Stage     string   `json:"stage"`
	Reason    string   `json:"reason"`
	RuleName  string   `json:"rule_name,omitempty"`
	Severity  string   `json:"severity,omitempty"`
	Tool      string   `json:"tool"`
	SessionID string   `json:"session_id"`
	// RiskScore is -1 when no model was consulted. Zero would be
	// indistinguishable from "a model looked and saw no risk".
	RiskScore int       `json:"risk_score"`
	ModelUsed string    `json:"model_used,omitempty"`
	Signals   []string  `json:"signals,omitempty"`
	Cost      float64   `json:"cost"`
	Forecast  Snapshot  `json:"forecast"`
	Message   string    `json:"message,omitempty"`
	At        time.Time `json:"at"`
}

// Deps are the firewall's collaborators. All are optional except Policies:
// a deployment with no ledger, no model, and no governance engine still
// enforces intent and cost.
type Deps struct {
	Logger     *slog.Logger
	Policies   *policy.Store
	Gov        *engine.GovernanceEngine
	Heal       *recovery.SelfHealingEngine
	Router     *llm.Router
	Ledger     *audit.Ledger
	Forecaster Forecaster
	// RecentSize bounds the in-memory decision ring served to the dashboard.
	RecentSize int
}

// Firewall is the decision pipeline.
type Firewall struct {
	deps     Deps
	sessions *Sessions

	mu     sync.Mutex
	recent []Result
}

func New(d Deps) *Firewall {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Policies == nil {
		d.Policies = policy.NewStore()
	}
	if d.Forecaster.HardLimitPct == 0 {
		d.Forecaster = DefaultForecaster()
	}
	if d.RecentSize <= 0 {
		d.RecentSize = 200
	}
	return &Firewall{deps: d, sessions: newSessions()}
}

// Policies exposes the policy store for the HTTP layer.
func (f *Firewall) Policies() *policy.Store { return f.deps.Policies }

// Router exposes the model router for the status endpoint.
func (f *Firewall) Router() *llm.Router { return f.deps.Router }

// Check decides whether a tool call may proceed.
//
// The ordering IS the security guarantee, so it is written as one readable
// sequence:
//
//  1. Declared intent. A BLOCK here returns immediately and is final.
//  2. Cost forecast on the *projected* charge. Hard limit blocks; soft limit
//     does not block, it recommends a cheaper route and raises scrutiny.
//  3. Behavioral plugins. CRITICAL blocks; HIGH and below raise scrutiny.
//  4. If nothing raised scrutiny, allow — with no model call at all.
//  5. Only now, a model, and only to make the answer stricter.
//
// A model can never be the reason something was allowed: every path that could
// permit a call has already returned before step 5.
func (f *Firewall) Check(ctx context.Context, c Call) Result {
	ctx, span := telemetry.StartDecision(ctx, c.SessionID, c.Tool)
	defer span.End()

	sess := f.sessions.Get(c.SessionID)
	pol := f.deps.Policies.GetOrDefault(c.SessionID, c.Budget)

	res := Result{
		Tool:      c.Tool,
		SessionID: c.SessionID,
		RiskScore: -1,
		Cost:      c.ToolCost,
		At:        time.Now(),
	}

	// --- 1. Declared intent --------------------------------------------------
	verdict := pol.Evaluate(c.Tool, c.Args)
	if verdict.Outcome == policy.Block {
		res.Decision, res.Stage, res.Reason = Block, StageIntent, verdict.Reason
		res.Message = "Vigil blocked this call: " + verdict.Reason
		return f.finish(ctx, span, sess, res)
	}

	// --- 2. Cost forecast ----------------------------------------------------
	// Forecast the charge this call *would* incur, not the one already spent —
	// the point is to intervene before the budget is gone.
	fc := f.deps.Forecaster.Compute(time.Now(), c.SessionCost+c.ToolCost, c.Budget, sess.costSamples())
	res.Forecast = fc

	tier := llm.TierNormal
	var signals []string

	if fc.State == StateHardLimit {
		res.Decision, res.Stage = Block, StageForecast
		res.Reason = fmt.Sprintf("budget exhausted: $%.4f of $%.2f", fc.CurrentCost, fc.Budget)
		res.Message = "Vigil blocked this call: " + res.Reason
		return f.finish(ctx, span, sess, res)
	}
	if fc.State == StateSoftLimit {
		// Not a block. A projected breach should reroute to something cheaper,
		// not kill an agent that is still doing useful work.
		signals = append(signals, "cost_soft_limit")
		tier = maxTier(tier, llm.TierSuspicious)
		f.recover(ctx, sess, c, engine.RuleResult{
			RuleName:        "Projected Budget Breach",
			Reason:          fmt.Sprintf("projected $%.4f against a $%.2f budget", fc.ProjectedTotal, fc.Budget),
			Severity:        engine.SeverityMedium,
			AutomaticAction: engine.ActionTriggerFallback,
		})
	}

	// --- 3. Behavioral plugins ----------------------------------------------
	if f.deps.Gov != nil {
		agentCtx := sess.AgentContext(c.Budget, c.Tool)
		for _, v := range f.deps.Gov.EvaluateContext(ctx, agentCtx) {
			signals = append(signals, v.RuleName)
			switch v.Severity {
			case engine.SeverityCritical:
				res.Decision, res.Stage = Block, StageBehavior
				res.RuleName, res.Severity, res.Reason = v.RuleName, string(v.Severity), v.Reason
				res.Signals = signals
				res.Message = "Vigil blocked this call: " + v.Reason
				return f.finish(ctx, span, sess, res)
			case engine.SeverityHigh:
				tier = maxTier(tier, llm.TierUncertain)
				res.RuleName, res.Severity = v.RuleName, string(v.Severity)
			default:
				tier = maxTier(tier, llm.TierSuspicious)
			}
		}
	}

	// Policy could not decide: exactly the case a model is for.
	if verdict.Outcome == policy.Uncertain {
		signals = append(signals, "intent_uncovered")
		tier = maxTier(tier, llm.TierUncertain)
	}
	res.Signals = signals

	// --- 4. Nothing raised scrutiny -----------------------------------------
	if tier == llm.TierNormal {
		res.Decision, res.Stage, res.Reason = Allow, StageDefault, verdict.Reason
		return f.finish(ctx, span, sess, res)
	}

	// --- 5. Model judgement, to tighten only --------------------------------
	if f.deps.Router == nil || !f.deps.Router.Available() {
		// No inference configured, so nothing can adjudicate the uncertainty.
		//
		// An UNCERTAIN intent verdict means the call fell outside a declared
		// allowlist. Allowing it here would make `allowed_tools` advisory on the
		// default configuration -- the operator wrote an allowlist and got a
		// suggestion. It fails closed instead, and says why.
		//
		// Other signals (a soft cost limit, a MEDIUM-severity detector) are
		// warnings rather than exclusions, so those still pass with the signal
		// recorded. Blocking on every signal when the provider is merely
		// unconfigured would make an unconfigured deployment unusable.
		if verdict.Outcome == policy.Uncertain {
			res.Decision, res.Stage = Block, StageIntent
			res.Reason = verdict.Reason + " (no judge configured to adjudicate it)"
			res.Signals = signals
			res.Message = "Vigil blocked this call: " + res.Reason
			return f.finish(ctx, span, sess, res)
		}
		res.Decision, res.Stage = Allow, StageDefault
		res.Reason = deterministicReason(signals, verdict.Reason)
		return f.finish(ctx, span, sess, res)
	}

	j, model, err := f.consult(ctx, tier, c, pol, sess, signals)
	if err != nil {
		// Fail closed on the strength of what determinism already found: a HIGH
		// signal we could not adjudicate becomes a block, anything less does
		// not. Never allow *because* the model failed.
		res.Stage = StageJudge
		res.Reason = fmt.Sprintf("model judgement unavailable (%v); deterministic signals: %s", err, strings.Join(signals, ", "))
		if tier >= llm.TierUncertain {
			res.Decision = Block
			res.Message = "Vigil blocked this call: " + res.Reason
		} else {
			res.Decision = Allow
		}
		return f.finish(ctx, span, sess, res)
	}

	res.Stage, res.ModelUsed, res.RiskScore = StageJudge, model, j.score()
	res.Severity = j.Severity
	res.Reason = j.reason()

	switch Decision(j.Decision) {
	case Block:
		res.Decision, res.Message = Block, "Vigil blocked this call: "+res.Reason
	case Pause:
		res.Decision, res.Message = Pause, "Vigil paused this session for review: "+res.Reason
	case Fallback:
		// Vigil does not run the agent's model, so it cannot execute a fallback
		// on the agent's behalf. The call proceeds and the recommendation is
		// surfaced; pretending otherwise would be a fake feature.
		res.Decision = Allow
		res.Reason = "model recommended a cheaper route: " + res.Reason
	default:
		res.Decision = Allow
	}
	return f.finish(ctx, span, sess, res)
}

// judgeBudget bounds the entire escalation stage.
//
// The per-attempt timeout does not bound this: a HighRisk tier walks two roles,
// each re-prompts once on a schema slip, each of those retries transient
// failures, and the chain repeats the whole thing per vendor. Multiplied out
// that is minutes, while the HTTP server's WriteTimeout is 15s — so the agent
// would get a dropped connection instead of a decision, which is a fail-open
// dressed as a network error.
//
// Ten seconds leaves headroom under WriteTimeout. Exceeding it is not an
// outage: the deadline surfaces as an error on the existing fail-closed path,
// so the call is decided on deterministic signals alone.
var judgeBudget = func() time.Duration {
	if v := vigil.Env("JUDGE_BUDGET_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 10 * time.Second
}()

// consult asks the model, retrying once on a validation failure.
func (f *Firewall) consult(ctx context.Context, tier llm.Tier, c Call, pol *policy.Policy, sess *Session, signals []string) (judgment, string, error) {
	ctx, cancel := context.WithTimeout(ctx, judgeBudget)
	defer cancel()

	prompt := f.buildPrompt(c, pol, sess, signals)

	var lastErr error
	var lastModel string
	for _, role := range f.deps.Router.RolesFor(tier) {
		if ctx.Err() != nil {
			// Out of budget. Stop rather than walking the remaining roles to
			// collect the same deadline error from each.
			return judgment{}, lastModel, ctx.Err()
		}
		user := prompt
		for attempt := 0; attempt < 2; attempt++ {
			mctx, mspan := telemetry.StartModelCall(ctx, string(role))
			resp, err := f.deps.Router.Complete(mctx, role, llm.Request{
				System: judgeSystemPrompt, User: user, MaxTokens: 500, Temperature: 0, JSONOnly: true,
			})
			var mc telemetry.ModelCall
			if resp != nil {
				mc = telemetry.ModelCall{
					ModelID: resp.ModelID, RequestID: resp.RequestID, Latency: resp.Latency,
					PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
				}
			}
			telemetry.RecordModelCall(mspan, mc, err)
			mspan.End()

			if err != nil {
				lastErr = err
				break // a transport failure will not be fixed by re-prompting
			}
			lastModel = resp.ModelID

			j, perr := parseJudgment([]byte(resp.Text))
			if perr == nil {
				return j, resp.ModelID, nil
			}
			lastErr = perr
			// Re-prompt once with the validation error appended. Models
			// frequently self-correct a schema slip when shown it.
			user = prompt + "\n\nYour previous reply was rejected: " + perr.Error() + "\nReturn only the required JSON object."
		}
	}
	if lastErr == nil {
		lastErr = llm.ErrNoModel
	}
	return judgment{}, lastModel, lastErr
}

func (f *Firewall) buildPrompt(c Call, pol *policy.Policy, sess *Session, signals []string) string {
	args, _ := json.Marshal(c.Args)
	if len(args) > 2048 {
		args = append(args[:2048], []byte("...[truncated]")...)
	}
	compiled, _ := json.MarshalIndent(pol, "", "  ")

	var history strings.Builder
	for _, sp := range sess.recentTools(10) {
		fmt.Fprintf(&history, "- %s (%s)\n", sp.Name, sp.Status)
	}
	if history.Len() == 0 {
		history.WriteString("- (none)\n")
	}

	intent := pol.DeclaredIntent
	if intent == "" {
		intent = "(no intent declared; permissive baseline in effect)"
	}

	return fmt.Sprintf(`Declared intent: %s

Requested tool: %s
Arguments: %s

Recent tool history:
%s
Budget: $%.2f, spent $%.4f, this call $%.4f

Compiled policy:
%s

Deterministic risk signals already detected: %s`,
		intent, c.Tool, string(args), history.String(),
		c.Budget, c.SessionCost, c.ToolCost, string(compiled),
		orNone(signals))
}

// Commit records a tool call that actually ran, so behavioral and cost history
// reflect executed work rather than attempted work.
func (f *Firewall) Commit(sessionID, tool string, cost float64, dur time.Duration, ok bool) {
	sess := f.sessions.Get(sessionID)
	status := "ok"
	if !ok {
		status = "error"
	}
	sess.RecordSpan(engine.TraceSpan{
		Name: tool, Kind: "tool", Duration: dur, Status: status,
	})
	sess.RecordCost(cost, time.Now())
}

// Forecast returns a session's current cost projection.
func (f *Firewall) Forecast(sessionID string, budget float64) Snapshot {
	sess := f.sessions.Get(sessionID)
	return f.deps.Forecaster.Compute(time.Now(), sess.Cost(), budget, sess.costSamples())
}

// Recent returns the most recent decisions, newest last.
func (f *Firewall) Recent(sessionID string, limit int) []Result {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Result, 0, len(f.recent))
	for _, r := range f.recent {
		if sessionID == "" || r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// DropSession clears a disconnected session's state.
func (f *Firewall) DropSession(id string) {
	f.sessions.Drop(id)
	f.deps.Policies.Drop(id)
}

// finish records the decision to telemetry, the audit chain, and the recent
// ring, then returns it. Every decision passes through here, including ALLOW.
func (f *Firewall) finish(ctx context.Context, span telemetry.Span, sess *Session, res Result) Result {
	telemetry.RecordDecision(span, string(res.Decision), res.Stage, res.Reason, res.RuleName, res.RiskScore, res.ModelUsed)

	// Record refused attempts in the behavioral history (but never charge for
	// them — Commit does cost, and only for calls that ran).
	//
	// Without this a tripped behavioral detector is inescapable: blocked calls
	// would never enter the span history, so the pattern that tripped it could
	// never age out of the window and every subsequent call would be refused
	// forever. Recording the attempt is also the more honest history — an agent
	// hammering a tool it keeps being denied is exactly the behavior worth
	// seeing.
	if res.Decision != Allow {
		sess.RecordSpan(engine.TraceSpan{Name: res.Tool, Kind: "tool", Status: "blocked"})
	}

	// Audit every decision, not just the blocks. A trail that records only
	// refusals proves nothing about what was allowed through.
	if f.deps.Ledger != nil {
		if _, err := f.deps.Ledger.Append(audit.Event{
			AgentID:   res.SessionID,
			SessionID: res.SessionID,
			Tool:      res.Tool,
			ArgsHash:  audit.HashArgs(nil),
			Decision:  string(res.Decision),
			Reason:    res.Reason,
			ModelUsed: res.ModelUsed,
			Cost:      res.Cost,
		}); err != nil {
			// An audit write failure is an error worth shouting about, but it
			// must not block the tool call: losing the record is bad, wedging
			// the agent because of it is worse.
			f.deps.Logger.ErrorContext(ctx, "vigil: audit append failed", slog.String("error", err.Error()))
		}
	}

	f.mu.Lock()
	f.recent = append(f.recent, res)
	if len(f.recent) > f.deps.RecentSize {
		f.recent = f.recent[len(f.recent)-f.deps.RecentSize:]
	}
	f.mu.Unlock()

	if res.Decision != Allow {
		f.deps.Logger.WarnContext(ctx, "vigil: call not allowed",
			slog.String("session", res.SessionID),
			slog.String("tool", res.Tool),
			slog.String("decision", string(res.Decision)),
			slog.String("stage", res.Stage),
			slog.String("reason", res.Reason),
		)
	}
	return res
}

// recover fires the self-healing engine for a synthesized violation.
func (f *Firewall) recover(ctx context.Context, sess *Session, c Call, v engine.RuleResult) {
	if f.deps.Heal == nil {
		return
	}
	f.deps.Heal.ExecuteRecovery(ctx, sess.AgentContext(c.Budget, c.Tool), v)
}

func maxTier(a, b llm.Tier) llm.Tier {
	if b > a {
		return b
	}
	return a
}

func deterministicReason(signals []string, fallback string) string {
	if len(signals) == 0 {
		return fallback
	}
	return "allowed with deterministic signals noted: " + strings.Join(signals, ", ")
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

// Behaviour returns a session's observed behavioural profile.
func (f *Firewall) Behaviour(sessionID string) Behaviour {
	return f.sessions.Get(sessionID).Behaviour()
}
