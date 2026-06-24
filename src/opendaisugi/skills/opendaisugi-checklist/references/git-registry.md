# Shared pathway registry (git-backed, v0.25+)

`PathwayStore` is per-machine sqlite. v0.25 lets multiple opendaisugi
instances share pathways through a **git repository** — any team that
already has a GitHub / GitLab / internal git remote gets a registry for
free. No server to operate; git is the transport.

## Repository layout

```
opendaisugi-registry/
├── trusted-signers.json        # JSON: {name → public_key_b64}
├── pathways/
│   ├── <bundle_hash>.yaml      # signed PathwayBundle, one per file
│   └── ...
└── README.md                   # optional team conventions
```

## Operator setup (one time)

```bash
# Create a registry repo on whichever git host the team uses.
gh repo create my-team/opendaisugi-registry --private
git clone git@github.com:my-team/opendaisugi-registry.git
cd opendaisugi-registry
echo '{}' > trusted-signers.json
mkdir pathways && touch pathways/.gitkeep
git add . && git commit -m 'initial' && git push
```

## Per-developer setup

```bash
# 1. Generate a signing keypair (one-time per developer).
python -c "
from opendaisugi.signing import generate_keypair
import pathlib
priv, pub = generate_keypair()
pathlib.Path('~/.opendaisugi/keys').expanduser().mkdir(parents=True, exist_ok=True)
pathlib.Path('~/.opendaisugi/keys/me.priv').expanduser().write_text(priv)
pathlib.Path('~/.opendaisugi/keys/me.pub').expanduser().write_text(pub)
print('public key (give this to your team):', pub)
"

# 2. Add the public key to trusted-signers.json (PR review flow recommended).
#    Edit the file in the registry repo, commit, push.

# 3. Clone the registry locally.
daisugi registry init git@github.com:my-team/opendaisugi-registry.git

# 4. Pull whatever's there now.
daisugi registry pull
```

## Daily flow

```bash
# Pulling: makes teammates' pathways available locally.
daisugi registry pull

# Publishing a reviewed pathway:
daisugi registry publish pathway_abc123 \
  --private-key ~/.opendaisugi/keys/me.priv \
  --public-key ~/.opendaisugi/keys/me.pub \
  --publisher alice@laptop

# Cron entry to keep everything fresh:
*/30 * * * * /usr/local/bin/daisugi registry pull-and-tend
```

## Programmatic use

```python
from opendaisugi import Daisugi
from opendaisugi.git_pathway_store import GitPathwayStore

store = GitPathwayStore(
    repo_path=Path("~/.opendaisugi/registry").expanduser(),
    private_key_b64=Path("~/.opendaisugi/keys/me.priv").read_text().strip(),
    public_key_b64=Path("~/.opendaisugi/keys/me.pub").read_text().strip(),
    publisher="alice@laptop",
    require_signed=True,
    offline_ok=True,
)
d = Daisugi(pathway_store=store)
```

`GitPathwayStore` subclasses `PathwayStore` — existing
`Daisugi(pathway_store=...)` integrations work unchanged when handed
one. Find / put / list_all retain their PathwayStore semantics; the
git layer adds `pull()`, `publish()`, `status()`.

## Trust boundaries

- **Signature gate**: `pull()` refuses bundles whose signing pubkey is
  not in `trusted-signers.json`. Adding a new signer is a PR to the
  registry repo — the team's normal review flow becomes the trust
  onboarding flow.
- **Tamper detection**: bundles are signed over their canonical-JSON
  body; any post-signature edit fails verification.
- **Privacy**: `CompiledPathway.publishable: bool = False` (default).
  `daisugi pathways mark-publishable` opts a specific pathway in
  before it can be published.

## What git gives us for free

| concern | git solves it via |
|---|---|
| Transport | `git pull` / `git push` |
| Provenance | commit history + ed25519 in bundle |
| Audit trail | `git log pathways/` |
| Access control | repo permissions |
| Conflict resolution | content-addressed filenames |
| Moderation queue | PRs (teams who want it have it) |
| Rollback | `git revert` |
| Cross-team sharing | fork the registry repo |
| Backup | every clone is a full backup |
| Offline mode | local clone keeps working |
