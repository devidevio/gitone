import fs from 'node:fs/promises';
import path from 'node:path';

import * as vscode from 'vscode';

import {
    checkProtocol,
    errorMessage,
    GitOneCli,
    VS_CODE_PROTOCOL,
} from './cli';
import { GitOneProvider, GitOneResource } from './provider';
import { VirtualDocument, VirtualDocuments } from './virtualDocuments';

export async function activate(
    context: vscode.ExtensionContext,
): Promise<void> {
    if (!vscode.workspace.isTrusted) {
        return;
    }
    const controller = new ExtensionController();
    context.subscriptions.push(controller);
    await controller.start(context);
}

export function deactivate(): void {}

class ExtensionController implements vscode.Disposable {
    private readonly output = vscode.window.createOutputChannel('GitOne');
    private readonly providers = new Map<string, GitOneProvider>();
    private readonly providersByRoot = new Map<string, GitOneProvider>();
    private readonly documents = new VirtualDocuments((document) =>
        this.readDocument(document),
    );
    private reconcileQueue: Promise<void> = Promise.resolve();
    private disposed = false;

    async start(context: vscode.ExtensionContext): Promise<void> {
        context.subscriptions.push(
            this.output,
            this.documents,
            vscode.workspace.registerTextDocumentContentProvider(
                'gitone',
                this.documents,
            ),
            vscode.workspace.onDidChangeWorkspaceFolders(() =>
                this.queueReconcile(),
            ),
            vscode.workspace.onDidChangeConfiguration((event) => {
                if (event.affectsConfiguration('gitone.path')) {
                    this.disposeProviders();
                    this.queueReconcile();
                }
            }),
            vscode.window.onDidChangeWindowState((state) => {
                if (state.focused) {
                    void this.refreshAll();
                }
            }),
            vscode.commands.registerCommand(
                'gitone.refresh',
                async (value?: unknown) => {
                    const provider = this.providerFor(value);
                    if (provider) {
                        await provider.refresh();
                    } else {
                        await this.refreshAll();
                    }
                },
            ),
            vscode.commands.registerCommand(
                'gitone.stage',
                (...values: unknown[]) => this.runForResources(values, 'stage'),
            ),
            vscode.commands.registerCommand(
                'gitone.unstage',
                (...values: unknown[]) =>
                    this.runForResources(values, 'unstage'),
            ),
            vscode.commands.registerCommand(
                'gitone.stageGroup',
                (...values: unknown[]) => this.runForResources(values, 'stage'),
            ),
            vscode.commands.registerCommand(
                'gitone.unstageGroup',
                (...values: unknown[]) =>
                    this.runForResources(values, 'unstage'),
            ),
            vscode.commands.registerCommand(
                'gitone.openFile',
                async (...values: unknown[]) => {
                    const resource = values.flatMap(flatten).find(isResource);
                    if (resource) {
                        await vscode.commands.executeCommand(
                            'vscode.open',
                            resource.openUri,
                        );
                    }
                },
            ),
            vscode.commands.registerCommand(
                'gitone.commit',
                async (value?: unknown) => {
                    const provider = await this.chooseProvider(
                        value,
                        'Commit in GitOne project',
                    );
                    await provider?.commit();
                },
            ),
            vscode.commands.registerCommand(
                'gitone.commitAndPush',
                async (value?: unknown) => {
                    const provider = await this.chooseProvider(
                        value,
                        'Commit and push GitOne project',
                    );
                    if (provider && (await provider.commit())) {
                        await provider.push();
                    }
                },
            ),
            vscode.commands.registerCommand(
                'gitone.push',
                async (value?: unknown) => {
                    const provider = await this.chooseProvider(
                        value,
                        'Push GitOne project',
                    );
                    await provider?.push();
                },
            ),
            vscode.commands.registerCommand(
                'gitone.recover',
                async (value?: unknown) => {
                    const provider = await this.chooseProvider(
                        value,
                        'Recover GitOne project',
                    );
                    await provider?.recover();
                },
            ),
            vscode.commands.registerCommand(
                'gitone.abort',
                async (value?: unknown) => {
                    const provider = await this.chooseProvider(
                        value,
                        'Abort GitOne operation',
                    );
                    await provider?.abort();
                },
            ),
            vscode.commands.registerCommand('gitone.showOutput', () =>
                this.output.show(true),
            ),
        );

        if (vscode.env.remoteName !== undefined) {
            await vscode.window.showWarningMessage(
                'GitOne supports local VS Code workspaces only.',
            );
            return;
        }
        if (process.platform !== 'darwin' && process.platform !== 'linux') {
            await vscode.window.showWarningMessage(
                'GitOne supports macOS and Linux only.',
            );
            return;
        }
        await this.reconcile();
    }

    dispose(): void {
        this.disposed = true;
        this.disposeProviders();
    }

    private queueReconcile(): void {
        this.reconcileQueue = this.reconcileQueue
            .then(() => this.reconcile())
            .catch((error) => {
                this.output.appendLine(String(error));
            });
    }

    private async reconcile(): Promise<void> {
        if (this.disposed) {
            return;
        }
        const wanted = new Set<string>();
        for (const folder of vscode.workspace.workspaceFolders ?? []) {
            if (folder.uri.scheme !== 'file') {
                continue;
            }
            const root = folder.uri.fsPath;
            if (!(await isProjectRoot(root))) {
                continue;
            }
            let canonical: string;
            try {
                canonical = await fs.realpath(root);
            } catch (error) {
                this.output.appendLine(`${root}: ${String(error)}`);
                continue;
            }
            const key =
                process.platform === 'darwin'
                    ? canonical.toLocaleLowerCase('en-US')
                    : canonical;
            wanted.add(key);
            if (this.providers.has(key)) {
                continue;
            }
            const executable = configuredExecutable(folder);
            if (executable instanceof Error) {
                await this.configurationFailure(executable.message);
                continue;
            }
            const cli = new GitOneCli(executable, root, this.output);
            try {
                const version = await checkProtocol(cli);
                this.output.appendLine(
                    `${folder.name}: GitOne ${version}, VS Code protocol ${VS_CODE_PROTOCOL}`,
                );
            } catch (error) {
                await this.configurationFailure(
                    `GitOne is unavailable for ${folder.name}: ${errorMessage(error)}`,
                );
                continue;
            }
            const provider = new GitOneProvider(
                root,
                folder.name,
                cli,
                this.documents,
                this.output,
            );
            this.providers.set(key, provider);
            this.providersByRoot.set(root, provider);
            await provider.refresh();
        }

        for (const [key, provider] of this.providers) {
            if (!wanted.has(key)) {
                provider.dispose();
                this.providers.delete(key);
                this.providersByRoot.delete(provider.root);
            }
        }
    }

    private disposeProviders(): void {
        for (const provider of this.providers.values()) {
            provider.dispose();
        }
        this.providers.clear();
        this.providersByRoot.clear();
    }

    private async refreshAll(): Promise<void> {
        await Promise.all(
            [...this.providers.values()].map((provider) => provider.refresh()),
        );
    }

    private providerFor(value: unknown): GitOneProvider | undefined {
        if (typeof value === 'string') {
            return this.providersByRoot.get(value);
        }
        if (isResource(value)) {
            return this.providersByRoot.get(value.projectRoot);
        }
        for (const provider of this.providers.values()) {
            if (provider.sourceControl === value || provider.ownsGroup(value)) {
                return provider;
            }
        }
        return this.providers.size === 1
            ? [...this.providers.values()][0]
            : undefined;
    }

    private async chooseProvider(
        value: unknown,
        placeHolder: string,
    ): Promise<GitOneProvider | undefined> {
        const direct = this.providerFor(value);
        if (direct) {
            return direct;
        }
        const items = [...this.providers.values()].map((provider) => ({
            label: provider.label,
            description: provider.root,
            provider,
        }));
        return (await vscode.window.showQuickPick(items, { placeHolder }))
            ?.provider;
    }

    private async runForResources(
        values: readonly unknown[],
        operation: 'stage' | 'unstage',
    ): Promise<void> {
        const resources: GitOneResource[] = [];
        for (const value of values.flatMap(flatten)) {
            if (isResource(value)) {
                resources.push(value);
                continue;
            }
            for (const provider of this.providers.values()) {
                resources.push(...provider.resourcesForGroup(value));
            }
        }
        const byProvider = new Map<GitOneProvider, GitOneResource[]>();
        for (const resource of resources) {
            const provider = this.providersByRoot.get(resource.projectRoot);
            if (provider) {
                const selected = byProvider.get(provider) ?? [];
                selected.push(resource);
                byProvider.set(provider, selected);
            }
        }
        for (const [provider, selected] of byProvider) {
            await provider[operation](selected);
        }
    }

    private async readDocument(document: VirtualDocument): Promise<string> {
        const provider = this.providersByRoot.get(document.root);
        if (!provider) {
            throw vscode.FileSystemError.Unavailable(
                'GitOne project is no longer open.',
            );
        }
        return provider.readSource(document);
    }

    private async configurationFailure(message: string): Promise<void> {
        this.output.appendLine(message);
        const answer = await vscode.window.showErrorMessage(
            message,
            'Open Settings',
            'Show Output',
        );
        if (answer === 'Open Settings') {
            await vscode.commands.executeCommand(
                'workbench.action.openSettings',
                'gitone.path',
            );
        } else if (answer === 'Show Output') {
            this.output.show(true);
        }
    }
}

async function isProjectRoot(root: string): Promise<boolean> {
    try {
        return (await fs.stat(path.join(root, '.gitone.yml'))).isFile();
    } catch {
        return false;
    }
}

function configuredExecutable(folder: vscode.WorkspaceFolder): string | Error {
    const configured = vscode.workspace
        .getConfiguration('gitone', folder.uri)
        .get<string>('path', '')
        .trim();
    if (configured === '') {
        return 'gitone';
    }
    if (!path.isAbsolute(configured)) {
        return new Error('gitone.path must be an absolute executable path.');
    }
    return configured;
}

function isResource(value: unknown): value is GitOneResource {
    if (value === null || typeof value !== 'object') {
        return false;
    }
    const resource = value as Partial<GitOneResource>;
    return (
        typeof resource.projectRoot === 'string' &&
        typeof resource.path === 'string' &&
        (resource.state === 'staged' || resource.state === 'working')
    );
}

function flatten(value: unknown): unknown[] {
    return Array.isArray(value) ? value : [value];
}
