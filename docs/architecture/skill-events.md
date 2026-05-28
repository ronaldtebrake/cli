# Skill Events Metadata

Entire stores raw agent transcripts unchanged. When an agent exposes an explicit skill signal, the CLI records a small sidecar annotation in the per-session checkpoint `metadata.json` so API/UI consumers can identify and optionally collapse the corresponding transcript event.

This is currently a proof-of-concept for:

- Claude Code `Skill` tool calls
- Pi `/skill:name` commands captured before Pi expands them

## Storage location

Skill events are stored on the session-level `CommittedMetadata`, not the root checkpoint summary:

```text
<checkpoint-id[:2]>/<checkpoint-id[2:]>/
├── metadata.json          # root CheckpointSummary, no skill_events
└── 0/
    ├── metadata.json      # session CommittedMetadata with skill_events
    ├── full.jsonl         # raw/redacted transcript
    └── prompt.txt
```

Wire fields:

```json
{
  "skill_events_version": 1,
  "skill_events": []
}
```

Both fields are omitted when no skill events were captured.

## Event shape

```json
{
  "id": "claude-skill-toolu_123",
  "event_type": "tool_invocation",
  "skill": {
    "name": "trigger-analysis"
  },
  "source": {
    "agent": "claude-code",
    "signal": "skill_tool_use",
    "confidence": "explicit"
  },
  "turn_id": "turn_abc123",
  "timestamp": "2026-05-25T12:34:56Z",
  "transcript_anchor": {
    "unit": "line",
    "start": 132,
    "end": 133,
    "entry_ids": ["assistant-msg-uuid"],
    "tool_use_id": "toolu_123"
  },
  "native": {
    "tool_name": "Skill",
    "tool_use_id": "toolu_123"
  },
  "collapse": {
    "target": "tool_pair",
    "label": "Skill: trigger-analysis",
    "default_collapsed": true
  }
}
```

### Fields

| Field | Meaning |
| --- | --- |
| `id` | Stable best-effort event ID. Used for de-duplication when present. |
| `event_type` | Type of skill signal. Current values: `tool_invocation`, `prompt_invocation`. |
| `skill.name` | Normalized skill name from the native agent signal. |
| `source.agent` | Agent registry name, e.g. `claude-code`, `pi`. |
| `source.signal` | Native signal used to create the event. Current values: `skill_tool_use`, `input_slash_command`. |
| `source.confidence` | Confidence in the signal. Current PoC only writes `explicit`. |
| `turn_id` | Entire turn ID when known. Used to associate skill events with the prompting turn. |
| `timestamp` | Native/runtime timestamp when available. Pi input events include this; Claude transcript extraction currently does not. |
| `transcript_anchor` | Best-effort location of the raw transcript event. Anchors are agent-format-specific. |
| `native` | Agent-specific fields preserved for debugging and future consumers. |
| `collapse` | UI hint describing what raw event can be collapsed by default. |

## Claude Code example

Claude Code exposes the safest signal: an assistant `tool_use` block whose tool name is `Skill` and whose input contains `skill`.

Raw transcript fragment:

```json
{
  "type": "assistant",
  "uuid": "a1",
  "message": {
    "content": [
      {
        "type": "tool_use",
        "id": "toolu_123",
        "name": "Skill",
        "input": { "skill": "trigger-analysis" }
      }
    ]
  }
}
```

Metadata event:

```json
{
  "id": "claude-skill-toolu_123",
  "event_type": "tool_invocation",
  "skill": { "name": "trigger-analysis" },
  "source": {
    "agent": "claude-code",
    "signal": "skill_tool_use",
    "confidence": "explicit"
  },
  "transcript_anchor": {
    "unit": "line",
    "start": 132,
    "end": 133,
    "entry_ids": ["a1"],
    "tool_use_id": "toolu_123"
  },
  "native": {
    "tool_name": "Skill",
    "tool_use_id": "toolu_123"
  },
  "collapse": {
    "target": "tool_pair",
    "label": "Skill: trigger-analysis",
    "default_collapsed": true
  }
}
```

Consumers should collapse the `Skill` tool call/result pair, not the original user prompt.

## Pi example

Pi expands `/skill:name` before the model turn starts. The Entire Pi extension listens to Pi's `input` event, which fires before skill expansion, and forwards the pending skill event in the next `before_agent_start` hook.

Raw user input:

```text
/skill:trigger-analysis check this diff
```

Metadata event:

```json
{
  "id": "pi-skill-trigger-analysis-2026-05-25T12:34:56Z-0",
  "event_type": "prompt_invocation",
  "skill": { "name": "trigger-analysis" },
  "source": {
    "agent": "pi",
    "signal": "input_slash_command",
    "confidence": "explicit"
  },
  "timestamp": "2026-05-25T12:34:56Z",
  "native": {
    "command": "/skill:trigger-analysis"
  },
  "collapse": {
    "target": "user_message",
    "label": "/skill:trigger-analysis",
    "default_collapsed": true
  }
}
```

Consumers may collapse the resulting skill-expanded user message/prompt.

## Non-goals

The CLI should not create default-collapsible skill events from weak transcript heuristics such as:

- reading a `SKILL.md` file
- paths containing `/skills/`
- expanded skill text appearing in a prompt
- system prompts listing available skills

Those signals may be useful for future low-confidence analysis, but this metadata format is intended to mark explicit skill events only.
