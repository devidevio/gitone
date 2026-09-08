# Developing the GitOne VS Code extension

The user-facing description lives in [README.md](README.md), which is also the
Marketplace page. This file covers building, debugging and packaging.

## Running from source

GitOne does not install development dependencies or the extension for you.
Run these commands yourself from this directory:

```bash
npm install
npm test
```

Then open `editors/vscode/` as the VS Code workspace, select **Run GitOne
Extension** in Run and Debug, and press F5. The Development Host opens the
disposable `/tmp/gitone-vscode-fixture` project using the files already
compiled by `npm test`. Create and initialize that fixture first as described
below. Configure an absolute binary path in
`GitOne: Path` when `gitone` is not already on the Development Host's `PATH`.

```bash
mkdir /tmp/gitone-vscode-fixture
mkdir /tmp/gitone-vscode-fixture/src
cd /tmp/gitone-vscode-fixture

cat > .gitone.yml <<'YAML'
version: 1
default_branch: main
repositories:
  app:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
YAML

printf '# VS Code fixture\n' > README.md
printf 'fixture\n' > src/example.txt
gitone init
```

Manual checks:

1. Open Source Control and confirm one GitOne provider with repository/state
   groups, native Git status letters/colors and issue entries.
2. Change a tracked file and open its SCM diff and gutter quick diff.
3. Stage, unstage and use Commit and Commit & Push through the SCM provider
   menu; confirm the working file is unchanged by unstage and a failed commit
   keeps its message.
4. Confirm Push and Abort require explicit confirmation, and that Recover,
   Refresh and the output channel remain available.
5. Add a second GitOne folder to the Development Host workspace and confirm a
   second independent provider appears without changing the built-in Git
   extension.

The implementation follows the official [Source Control API](https://code.visualstudio.com/api/extension-guides/scm-provider)
and is disabled in [Restricted Mode](https://code.visualstudio.com/api/extension-guides/workspace-trust)
because it executes the GitOne CLI.

## Local VSIX

Package and install a local development build only with commands you run
yourself:

```bash
cd editors/vscode
npm install
npm test
npm run package -- --out gitone-0.1.0.vsix
gitone vscode install --vsix ./gitone-0.1.0.vsix --force
```

The last command delegates to `code --install-extension`; `--force` is needed
only when replacing the same local development version. Reload the VS Code
window afterwards. `gitone setup` may offer the Marketplace extension after a
project setup succeeds, defaults to no and never forces an installation.
