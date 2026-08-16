# VIGIL Python SDK

The official 1-line auto-instrumentation SDK for VIGIL - the Autonomous Reliability Guardian for AI Agents.

## Installation

```bash
pip install vigil-sdk
```

## Usage

Simply call `vigil.init()` at the start of your application. The SDK will automatically detect and instrument your AI frameworks, capturing:
- Tool calls
- Prompts & Completions
- Token usage & Latency
- Trace ID, Session ID, User ID
- Exceptions

```python
import vigil
from openai import OpenAI

# 1-line auto-instrumentation
vigil.init(
    project_name="customer-support-agent",
    endpoint="http://localhost:4317",
    budget_limit=10.0 # Circuit breaker limit per run
)

client = OpenAI()
# The following call is automatically traced and sent to the VIGIL control plane
response = client.chat.completions.create(
    model="gpt-4",
    messages=[{"role": "user", "content": "Hello!"}]
)
```

## Supported Frameworks
The VIGIL SDK seamlessly instruments:
- OpenAI SDK
- LangChain
- LangGraph
- LlamaIndex
- CrewAI

## Semantic Conventions
We emit standard OpenTelemetry GenAI semantic conventions, allowing VIGIL to construct Agent DNA fingerprints and monitor cost firewalls in real-time.
