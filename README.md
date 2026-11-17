# mealie-extras

Companion tools for a [Mealie](https://docs.mealie.io) instance.

| Command | Purpose |
| --- | --- |
| `mealie-generate` | Generate a random meal plan. |
| `mealie-shoppinglist` | Add planned recipes' ingredients to a shopping list. |
| `mealie-pantry` | Web app tracking what is in the fridge, freezer and pantry. |

Build all three with `make build` (binaries in `bin/`).

`mealie-generate` and `mealie-shoppinglist` accept `--ntfy-token`, or the
`NTFY_TOKEN` environment variable, for ntfy authorization. If it is omitted,
the Mealie API key is used for backward compatibility.

## mealie-pantry

A standalone web app with its own SQLite database. It only **reads** from
Mealie; it never changes recipes, meal plans or shopping lists.

- **Stock**: lots per location (fridge, freezer, pantry) with quantity, unit and
  optional expiry date. Use the quick ±1 buttons or the “More” menu to use an
  amount, move a lot, set its expiry or remove it. Lots that expire within three
  days are listed at the top.
- **Add**: add items manually. A name that matches a Mealie food (case-insensitive)
  is linked to it, so cooked meals subtract it. Other names are stored as free text.
- **Purchases (automatic)**: items checked off on the watched shopping list(s)
  appear in the **Inbox**. Choose Fridge, Freezer or Pantry (with an optional
  quantity override and expiry date), or Ignore. The last location chosen for
  each food is preselected next time, and “Store all in suggested places” accepts
  everything. Unchecking an item before it is stored removes it from the Inbox.
- **Cooked meals (automatic)**: “I Made This” on a recipe in Mealie is matched
  to the meal-plan entry for that recipe on the same day (or ±1 day). The recipe's
  ingredients are then subtracted from stock. You can also press **Mark cooked**
  on the Meals page. Each plan entry is subtracted at most once.
- **Subtraction rules**: ingredients match stock by Mealie food and unit, and are
  taken from the fridge first, then the pantry, then the freezer. Within a
  location, the soonest-expiring lot is used first. Stock never goes below zero.
  Recipe quantities are used as written; they are not scaled by servings.
  Anything that cannot be subtracted goes to **Review**: unlinked ingredients,
  untracked foods, a missing quantity, a unit mismatch or a shortfall. There you
  can subtract manually or dismiss it.
- **History**: every change (manual, purchase, cooked) can be undone.

### Configuration

Each flag falls back to an environment variable when it is not given.

| Flag | Env | Default | |
| --- | --- | --- | --- |
| `--apikey` | `API_KEY` | (required) | Mealie API token (read access suffices) |
| `--mealie-url` | `MEALIE_URL` | `https://mealie.tp-softworks.se` | Base URL, without `/api` |
| `--list` | `SHOPPING_LISTS` | `704f7c3f-…` | Shopping list ID(s); repeat the flag or separate with commas |
| `--listen` | `LISTEN` | `:8080` | |
| `--db` | `DB_PATH` | `pantry.db` | SQLite file |
| `--poll-interval` | `POLL_INTERVAL` | `5m` | Fallback sync interval |
| `--hook-secret` | `HOOK_SECRET` | (empty: hook disabled) | Secret in the notifier URL |
| `--timezone` | `TZ` | local | Zone used for meal-plan dates |
| `--loglevel` | `LOGLEVEL` | `INFO` | |

The app has **no login of its own**. Run it behind your reverse proxy's
authentication, and leave `/hooks/mealie/` reachable by Mealie. That path is
protected by its secret. Form posts from other sites are refused.

### Instant purchase sync (Mealie notifier)

Polling picks up purchases within `--poll-interval`. To make them immediate,
create a notifier in Mealie (*Household → Notifiers*):

- Apprise URL: `jsons://pantry.example.com/hooks/mealie/<HOOK_SECRET>`
  (use `json://` for plain HTTP)
- Enable the **Shopping list updated** event.

The notification body is ignored; it only triggers a re-sync.

### Docker

```sh
docker build -t mealie-pantry .
docker run -d --name mealie-pantry -p 8080:8080 -v pantry-data:/data \
  -e API_KEY=… -e HOOK_SECRET=$(openssl rand -hex 16) -e TZ=Europe/Stockholm \
  mealie-pantry
```

The database lives at `/data/pantry.db`. Back up the volume.
