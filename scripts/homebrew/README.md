# Local Homebrew service

This packages the current macOS checkout, including its built management UI, into a
standalone `bifrost-local` formula in the **local** `maple/bifrost-local` tap. It does
not upload a release or require a remote GitHub tap. Go, Node, Python 3 and Homebrew
are build/install dependencies; the installed service needs none of the development
servers, Python, Go or Node at runtime. The launcher uses macOS's system OpenSSL to
create its initial random setup token.

From the repository root:

```bash
scripts/homebrew/install-local.sh
brew test bifrost-local
brew services start bifrost-local
```

Open http://127.0.0.1:8080 and use `bifrost-local setup-token` to read the token for
creating the initial administrator. Reconnect ChatGPT and CodeBuddy in Providers.
The service binds to loopback and uses a new profile, independently of
`tests/integrations/python` and its backing services. Inference authentication is
enabled; configure credentials/virtual keys for API clients in the dashboard.

## Service controls

```bash
brew services start bifrost-local    # Start now and automatically at user login
brew services stop bifrost-local     # Stop and disable automatic startup
brew services restart bifrost-local
brew services info bifrost-local
brew services run bifrost-local      # Run now without registering login startup
```

Use these without `sudo`, under the same macOS user as the browser and CodeBuddy
desktop app. No Vite, Air, Weaviate, or repository working directory is required
when running the installed service. The macOS login callback port 1455 must be
available when connecting ChatGPT.

- Data: `~/Library/Application Support/BifrostLocal` (0700).
- Initial configuration and setup-token file: 0600; initialized once without
  overwriting existing files. The config references the setup token by environment
  variable rather than embedding its value.
- Account credentials: the normal provider manager's `accounts/` subdirectory.
- Standard output: `$(brew --prefix)/var/log/bifrost-local.log`.
- Standard error: `$(brew --prefix)/var/log/bifrost-local-error.log`.
- Local source archives/formula: `<checkout>/tmp/homebrew/` (Git-ignored).

To run manually on another port or with another profile:

```bash
BIFROST_PORT=8081 BIFROST_APP_DIR="$HOME/Library/Application Support/BifrostAlternate" bifrost-local
```

`BIFROST_HOST` defaults to `127.0.0.1`; command-line arguments passed to
`bifrost-local` follow these defaults and can override them. For persistent service
overrides, Homebrew supports its per-service environment configuration; see the
[Homebrew service API](https://docs.brew.sh/rubydoc/Homebrew/Service.html).

## Update or remove

After changing the checkout, rebuild/reinstall and restart:

```bash
scripts/homebrew/install-local.sh
brew services restart bifrost-local
```

The local tap has no remote update source: `brew upgrade` alone cannot obtain new
project code. The generated formula refers to a checksummed local tarball; keep it
for a later `brew reinstall`, or rerun the installer to generate a new one.

```bash
brew services stop bifrost-local
brew uninstall bifrost-local
```

Reinstall/uninstall leaves the data directory and account credentials intact.
Remove that directory manually only if the profile should be permanently deleted.

For debugging packaging, `--package-only` builds the archive/formula without
modifying Homebrew; `--install-only` installs the most recently prepared package.
