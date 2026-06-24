# Step Metadata Vocabulary

Canonical keys per step `type`. Agents should populate these exact fields so
envelope predicates can select against them reliably.

Unknown keys are allowed but emit a `UserWarning` — graduate stable patterns
to typed Pydantic subclasses as they solidify.

## `email_send`
- `to: str | list[str]` — recipient address(es)
- `from: str` — sender address
- `signature: str` — name that signs off the body
- `subject: str`
- `body: str` — full email body, including any salutation/sign-off

## `imessage_send`
- `to: str` — recipient phone or handle
- `body: str`

## `shell` (core type — `command` is already a typed field)
- `output: str` — stdout captured post-execution (populated by Supervisor)

## `file_write` (core type — `path` and `content` are typed)
- `author: str` — which agent created the content

## `network` (core type — `url`, `method`, `headers` typed)
- `response_body: str` — populated post-execution for Stage 2 checks

## `council_vote` (proposed)
- `approve: bool`
- `reasoning: str`
- `output: str` — structured JSON of `{approve, reasoning}`

## `kb_contribute` (proposed)
- `path: str` — where in the KB the contribution should land
- `body: str` — the contribution content
- `author: str`
