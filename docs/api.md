# Fribo portal API

What fribo-mcp relies on, worked out from the customer portal's public JavaScript
(portal.hyttetjenester.no) and confirmed with read-only calls in November 2026 unless noted.
This is not an official API.

## Basics

- Base URL: `https://portal.hyttetjenester.no/__platform`, proxied by the portal's own host.
  Other prefixes exist (`/__resources`, `/__messaging`, `/__payment`).
- JSON in and out. Some endpoints need an `api-version` header (`1.0` or `2.0`); the version
  for each call is listed below. Endpoints without one are called without the header.
- Errors: HTTP 4xx with `{"code": "...", "message": "...", "data": {}}`. fribo-mcp explains
  the stay-related codes (see `errorHints` in `fribo.go`).
- Times are ISO 8601 UTC. The portal uses the browser's local time (Europe/Oslo).

## Login

`POST /tokens/emails` with `{"email", "password", "applicationId"}`. The application id is a
constant in the portal bundle (`VITE_APPLICATION_ID`). No SMS step for e-mail login.

Answer: `{"accessToken", "accessTokenExpiration", "refreshTokenExpiration"}`, plus a
`refresh_token` cookie.

- The access token is a JWT that lasts 5 minutes. `sub` is the user id. Other claims hold the
  user's name, e-mail, phone number, role and scopes.
- `POST /platform/tokens/refresh` with the cookie returns a new access token and **rotates**
  the refresh token, which lasts 24 hours. fribo-mcp does not use it and logs in again instead.
- `GET /platform/users/me` answers 405; the user id comes from the token.

## Cabins and contracts

| Call | Version | Notes |
|---|---|---|
| `GET /cabins/byOwner/{userId}` | – | `[{id, name, address, isDefault}]` |
| `GET /cabins/{cabinId}/withUsers` | – | Owners and co-owners with contact details. Not used |
| `GET /customers/profiles/{userId}` | – | Profile and cabin roles. Not used |
| `GET /plowing/{cabinId}/cabinContractPreview?validByDate=true` | 2.0 | Agreements: `id, contractId, status, name, type, organizationId, startDate, endDate, lastPlowed?, usedClips?, boughtClips?` |
| `GET /plowing/{organizationId}/configurations` | – | Season, deadlines, first plowing hour |

Agreement types include `PlowingContract`, `PermanentStay` and `CommonRoad`. Like the portal,
fribo-mcp registers stays against a `PermanentStay`, else the first agreement that is not
`CommonRoad`.

### Plowing configuration

- `startSeasonDate`, `endSeasonDate`: the plowing season. In November 2026 the test account's
  season still ended 2026-06-30, yet stays for November were accepted and guaranteed, both from
  the portal and from fribo-mcp. fribo-mcp only mentions it as a note.
- `plowingSchedule.workDaysDeadline` and `weekendsDeadline`, each with `arrivalDeadline`
  (days), `arrivalDeadlineHours` (a timestamp whose local clock time is the cut-off),
  optional `arrivalDeadlineWeekday`, `departureDeadline` and `departureDeadlineHours`.
- Weekend deadlines apply to Friday, Saturday, Sunday and `workingExceptionDays`.
- Rule (portal's `deadline-info-card` code): a day can be ordered when it is at least
  `arrivalDeadline` days after today, or one more day once today's cut-off has passed.
- `firstHourPlowing`: the arrival hour cannot be earlier. `possibleArrivalHours`: allowed
  hours, empty means all.
- `allowDeadlineOrder`: whether late orders are accepted. With `false`, late stays were still
  accepted in practice but marked `plowingGuaranteed: false`.

## Stays (customer presences)

| Call | Version | Body |
|---|---|---|
| `GET /plowing/customerPresences/{cabinId}?page=1&size=N` | 2.0 | – |
| `GET /plowing/customerPresences/{cabinId}/{stayId}` | 2.0 | – |
| `GET /plowing/customerPresences/{cabinId}/preview?page=1&size=500&staysMode=OnlyFuture&cabinContractId=…` | 2.0 | – |
| `POST /plowing/customerPresences` | 2.0 | `{cabinId, visitingCustomerId, arrival, departure, rejectedDays, comment: null}` |
| `PUT /plowing/customerPresences/{stayId}` | 2.0 | `{arrival, departure, rejectedDays, comment: null}` |
| `DELETE /plowing/customerPresences/{stayId}` | 2.0 | – |

- **Upcoming stays are only in `/preview`.** In November 2026 the plain list returned the 11
  stays from the 2025/26 winter and none of the two new ones, with or without
  `cabinContractId`/`staysMode`. `/preview` with `staysMode=OnlyFuture` and the agreement id from
  `cabinContractPreview` listed both (`All`/`OnlyPast` answer 400). The preview has only
  `id, arrivalDate, departureDate, hasRejectedDates, presenceDatesEdited, visitingCustomer`,
  so fribo-mcp reads each stay with the details call.
- The plain list's pages start at 1 (page 0 gives the same as page 1). It ignored `size` and
  returned all stays on page 1; page 2 was empty.
- A stay: `id, cabinId, arrival, departure, rejectedDates, plowingGuaranteed,
  clipWasConsumed, visitingCustomer {id, firstName, lastName}, createdAt, lastUpdated`.
- `arrival` is the arrival hour, local time. `departure` is 23:59:59 local on the departure day.
  `rejectedDays` are `YYYY-MM-DD` days within the stay that need no plowing.
- `visitingCustomerId` is the user registering the stay. Co-owners register their own.
- Create was confirmed against the real API on 2026-11-02: HTTP 200 without the new stay in the
  answer, so fribo-mcp looks it up through `/preview` afterwards. The stay was guaranteed even
  though the season in the plowing configuration had ended (see below). Update and delete have
  not been called against the real API yet; their bodies and methods come from the portal code.

## History

| Call | Version | Notes |
|---|---|---|
| `GET /plowing/cabin/{cabinId}/history?page=1&size=N` | 2.0 | `eventDate, plowingEventType, clipConsumed, user {fullName, email, phoneNumber}` (the driver). Pages start at 1 and `size` is respected |
| `GET /plowing/{cabinId}/cabinClipsHistory?page=1&size=250` | 1.0 | Clip changes, for clip-based contracts |
| `GET /plowing/{cabinId}/cabinContracts/histories?page=1&pageSize=10` | 1.0 | Past agreements |

Event types: `Plow`, `MaintenancePlow`, `Pave`, `Inspect`, `Incident`, `Obstacle`,
`NeedsPaving`, `RejectPlowing`, `RequestPlowing`, `RevertRejection`, `PlowingNotNeeded`,
`MaintenancePlowRequest`. fribo-mcp drops the driver details.

## Not used yet

- Plowing instructions: `GET`/`PUT /plowing/{cabinId}/instructions?type=Customer` (2.0).
- Inspection and heating: `POST /inspection/cabins/availability`, `/inspection/cabin/{id}/heating`.
- Firewood: `POST /firewood/cabins/availability`, `/firewood/cabin/{id}/orders`.
- Notifications: `/__messaging/messaging/notifications/{userId}`.
- Payments and clip packages under `/__payment`.
