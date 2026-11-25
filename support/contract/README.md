# support/contract

Single source of truth of the support escalation contract shared by the Hub,
OPS instances, the desk addon and external clients (7 Leguas, PHP). Design:
`docs/PLAN-SOPORTE-UNIFICADO.md` section 3 of the Hub repository.

Standard library only. No I/O. Import path:
`github.com/asteby/metacore-kernel/support/contract`.

## What is here

| File | Content |
|---|---|
| `types.go` | Wire types: `EscalateRequest/Response`, `MessageRequest/Response`, `LinkRequest/Response`, `LinkAckRequest`, `CallbackEvent`, `DeskIngestEvent`, `InstanceContext`, `ErrorResponse` |
| `consts.go` | Events, link source/state, severities, channels, header names, paths |
| `errors.go` | Stable error codes, `not_linked` reasons, canonical HTTP status per code, `APIError` |
| `sign.go` | `SignBody`, `SignCallback`, `VerifyCallback`, `AcceptCallback`, `AckProof`, `VerifyAckProof`, `DeliveryDeduper` |
| `dedupe.go` | `MemoryDeduper`, an in-process `DeliveryDeduper` |
| `validate.go` | `ValidFolio`, `ValidMessageID`, `ValidPriority` (same expressions as the Hub; patterns exported) |
| `version.go` | `ContractV1`, `ContractV2`, `X-Support-Contract` negotiation |
| `channels.go` | Channel fields of the desk case (`ChannelRef`, `Requester`), thread views (`CaseView`, `MessageView`), `ProjectEvent`, `VisitorErasedEvent` |
| `embed.go` | `EmbedSessionClaims` and `EmbedSessionPurpose` (`support.embed.session`) |
| `profile.go` | `PublicProfile` and `Validate()` (colour, https URLs, text limits) |
| `testdata/*.json` | Golden fixtures, one per request, response and event, plus `signature_vectors.json` |

## Channels (additive, still contract v2)

A channel adds only optional fields and new events, so `ContractCurrent` stays 2 and the v1/v2 fixtures are unchanged:

- `DeskCase.case_id`, `.channel{id,public_id,name,kind}`, `.requester{kind,verified,display_name,user_ref,email?}`; `InstanceContext.plan` and `.last_heartbeat`.
- `visitor_erased` is a desk event (`DeskEvents`), body `VisitorErasedEvent`; never a callback event.
- `ProjectEvent(CallbackEvent)` returns the thread message, the case status the event implies (empty when unchanged) and whether the event is known. Unknown events must still be acknowledged.
- `PublicProfile.Validate()` returns `ProfileErrors`. The Hub still checks colour contrast and that the logo host is an allowed origin.
- Channel error codes: `origin_not_allowed` 403, `channel_inactive` 403 (with `reason`), `session_expired` 401, `identity_invalid` 401, `captcha_required` 428, `channel_quota_exceeded` 402, `channel_ai_cap_reached` 402, `desk_not_verified` 409.
- Fixtures for code generation (SDK): `thread_case.json`, `thread_message.json`, `embed_session_claims.json`, `public_profile.json`, `visitor_erased.json`, `desk_case_opened_channel.json`, `error_channel_inactive.json`.

## Callback signature

```
X-Hub-Signature: sha256=<hex HMAC-SHA256(secret, raw body)>
X-Hub-Timestamp: <unix seconds>      must equal body.timestamp, +-300 s of now
X-Hub-Delivery:  <delivery id>       must equal body.delivery_id
X-Hub-Event:     <event>
```

PHP receiver:

```php
$ok = hash_equals('sha256=' . hash_hmac('sha256', $raw, $secret), $header);
```

The vectors in `testdata/signature_vectors.json` were computed with `openssl`
and PHP, not with Go. Receivers must deduplicate `delivery_id` for 7 days
(`DedupeTTL`) after the signature verified, never before, and must answer 2xx to
events they do not know.

Link acknowledgement: `proof = hex(HMAC-SHA256(secret, instance_id || ts))`,
where `||` is concatenation and `ts` is the decimal `X-Signature-Timestamp` of the
`/link/ack` request itself.

## Version policy

- `ContractV1` is the original contract (manual link, GitHub issue). It is
  frozen: a v1 body decodes into these types and a v1 response encodes
  byte-for-byte the same (`TestBackwardCompatV1`).
- `ContractV2` is v1 plus optional fields. Within a version only optional
  fields (`omitempty`) and new events are added; nothing is renamed, retyped
  or removed. Anything else needs `ContractV3`.
- Negotiation: header `X-Support-Contract` (wins) or body `contract`; missing
  or malformed means v1 (`EffectiveContract`).
- Receivers ignore unknown fields and unknown events. Do not enable
  `DisallowUnknownFields` outside tests.
- Error codes and event names are wire values and never change; new ones may be
  added. `not_linked` gains a `reason`, never a new code.
- Retiring v1 requires every v1 client to declare `contract: 2` and a
  90 day minimum window.
- Fixtures: every new field or event adds or updates a fixture;
  `TestFixturesRoundTrip` fails if a fixture has no case or does not round
  trip through the Go types. Other languages (PHP, TypeScript) copy the fixtures
  and test against them.
