import path from 'node:path';

import * as vscode from 'vscode';

import { CliError, errorMessage, GitOneCli, isLockError } from './cli';
import {
    ChangeStatus,
    groupChanges,
    ignoredColor,
    ignoredPath,
    parseStatus,
    ProjectIssue,
    ProjectStatus,
    statusDecoration,
} from './model';
import { VirtualDocument, VirtualDocuments } from './virtualDocuments';

export interface GitOneResource extends vscode.SourceControlResourceState {
    readonly projectRoot: string;
    readonly repository: string;
    readonly state: 'staged' | 'working';
    readonly path: string;
    readonly openUri: vscode.Uri;
}

export class GitOneProvider
    implements vscode.Disposable, vscode.FileDecorationProvider
{
    readonly sourceControl: vscode.SourceControl;
    readonly onDidChangeFileDecorations: vscode.Event<vscode.Uri[] | undefined>;

    private readonly groups = new Map<
        string,
        vscode.SourceControlResourceGroup
    >();
    private readonly disposables: vscode.Disposable[] = [];
    private readonly workingPaths = new Map<string, string>();
    private readonly fileDecorationEmitter = new vscode.EventEmitter<
        vscode.Uri[] | undefined
    >();
    private fileDecorations = new Map<string, vscode.FileDecoration>();
    private ignored: readonly string[] = [];
    private ignoreExceptions: readonly string[] = [];
    private refreshTimer: NodeJS.Timeout | undefined;
    private refreshing: Promise<void> | undefined;
    private refreshRequested = false;
    private busy = false;
    private disposed = false;
    private mutation: Promise<void> = Promise.resolve();
    private issueSignature = '';
    private errorSignature = '';
    private lockRetryScheduled = false;
    private stagedCount = 0;

    constructor(
        readonly root: string,
        readonly label: string,
        private readonly cli: GitOneCli,
        private readonly documents: VirtualDocuments,
        private readonly output: vscode.OutputChannel,
    ) {
        this.sourceControl = vscode.scm.createSourceControl(
            'gitone',
            `GitOne: ${label}`,
            vscode.Uri.file(root),
        );
        this.onDidChangeFileDecorations = this.fileDecorationEmitter.event;
        this.sourceControl.inputBox.placeholder = commitPlaceholder;
        this.sourceControl.acceptInputCommand = {
            command: 'gitone.commit',
            title: 'Commit',
            arguments: [root],
        };
        this.sourceControl.quickDiffProvider = {
            provideOriginalResource: (uri) => this.originalResource(uri),
        };

        const watcher = vscode.workspace.createFileSystemWatcher(
            new vscode.RelativePattern(root, '**/*'),
        );
        const lockWatcher = vscode.workspace.createFileSystemWatcher(
            new vscode.RelativePattern(path.join(root, '.gitone'), 'lock'),
            true,
            true,
            false,
        );
        this.disposables.push(
            this.fileDecorationEmitter,
            vscode.window.registerFileDecorationProvider(this),
            watcher,
            lockWatcher,
            watcher.onDidCreate((uri) => this.fileChanged(uri)),
            watcher.onDidChange((uri) => this.fileChanged(uri)),
            watcher.onDidDelete((uri) => this.fileChanged(uri)),
            lockWatcher.onDidDelete(() => this.scheduleRefresh()),
        );
    }

    scheduleRefresh(delay = 250): void {
        if (this.refreshTimer) {
            clearTimeout(this.refreshTimer);
        }
        this.refreshTimer = setTimeout(() => {
            this.refreshTimer = undefined;
            void this.refresh();
        }, delay);
    }

    async refresh(): Promise<void> {
        this.refreshRequested = true;
        if (this.busy || this.disposed) {
            return;
        }
        if (this.refreshing) {
            return this.refreshing;
        }
        this.refreshing = (async () => {
            while (this.refreshRequested && !this.busy && !this.disposed) {
                this.refreshRequested = false;
                await this.loadStatus();
            }
        })().finally(() => {
            this.refreshing = undefined;
        });
        return this.refreshing;
    }

    ownsGroup(value: unknown): boolean {
        return [...this.groups.values()].some((group) => group === value);
    }

    resourcesForGroup(value: unknown): GitOneResource[] {
        for (const group of this.groups.values()) {
            if (group === value) {
                return [...group.resourceStates] as GitOneResource[];
            }
        }
        return [];
    }

    provideFileDecoration(uri: vscode.Uri): vscode.FileDecoration | undefined {
        const decoration = this.fileDecorations.get(uri.toString());
        if (decoration) {
            return decoration;
        }
        const relative = this.projectPath(uri);
        if (
            relative === undefined ||
            !ignoredPath(this.ignored, this.ignoreExceptions, relative)
        ) {
            return undefined;
        }
        return ignoredDecoration;
    }

    async stage(resources: readonly GitOneResource[]): Promise<void> {
        const paths = selectedPaths(resources, this.root, 'working');
        if (paths.length > 0) {
            await this.runMutation(['add', ...paths.map(cliPath)]);
        }
    }

    async unstage(resources: readonly GitOneResource[]): Promise<void> {
        const paths = selectedPaths(resources, this.root, 'staged');
        if (paths.length > 0) {
            await this.runMutation(['unstage', ...paths.map(cliPath)]);
        }
    }

    async commit(): Promise<boolean> {
        const message = this.sourceControl.inputBox.value;
        if (message.trim() === '') {
            await vscode.window.showErrorMessage(
                'Enter a commit message first.',
            );
            return false;
        }
        if (this.stagedCount === 0) {
            await vscode.window.showInformationMessage(
                'GitOne has no staged changes to commit.',
            );
            return false;
        }
        if (await this.runMutation(['commit', '-F', '-'], message)) {
            this.sourceControl.inputBox.value = '';
            return true;
        }
        return false;
    }

    async push(): Promise<void> {
        const answer = await vscode.window.showWarningMessage(
            `Push all outgoing GitOne commits for ${this.label}?`,
            { modal: true },
            'Push',
        );
        if (answer === 'Push') {
            await this.runMutation(['push', '--yes']);
        }
    }

    async recover(): Promise<void> {
        await this.runMutation(['recover']);
    }

    async abort(): Promise<void> {
        const answer = await vscode.window.showWarningMessage(
            `Abort the interrupted GitOne operation for ${this.label}?`,
            { modal: true },
            'Abort',
        );
        if (answer === 'Abort') {
            await this.runMutation(['abort']);
        }
    }

    async readSource(document: VirtualDocument): Promise<string> {
        try {
            const result = await this.cli.run(
                [
                    'show',
                    '--repository',
                    document.repository,
                    '--source',
                    document.source,
                    '--',
                    cliPath(document.path),
                ],
                { waitForLock: true },
            );
            return result.stdout.toString('utf8');
        } catch (error) {
            if (error instanceof CliError && error.exitCode === 2) {
                return '';
            }
            throw error;
        }
    }

    dispose(): void {
        this.disposed = true;
        if (this.refreshTimer) {
            clearTimeout(this.refreshTimer);
        }
        for (const group of this.groups.values()) {
            group.dispose();
        }
        for (const disposable of this.disposables) {
            disposable.dispose();
        }
        this.sourceControl.dispose();
    }

    private async loadStatus(): Promise<void> {
        try {
            const result = await this.cli.run(['status', '--json'], {
                acceptedExitCodes: [0, 1],
            });
            let status: ProjectStatus;
            try {
                status = parseStatus(result.stdout.toString('utf8'));
            } catch (error) {
                if (result.stderr) {
                    throw new Error(result.stderr);
                }
                throw error;
            }
            this.applyStatus(status);
            this.errorSignature = '';
            this.lockRetryScheduled = false;
        } catch (error) {
            const message = errorMessage(error);
            if (isLockError(error)) {
                // One quiet retry per contention. The lock watcher refreshes
                // again once the lock is released, so polling adds nothing.
                if (!this.lockRetryScheduled) {
                    this.lockRetryScheduled = true;
                    this.scheduleRefresh(1_000);
                }
                return;
            }
            this.lockRetryScheduled = false;
            this.sourceControl.statusBarCommands = [
                {
                    command: 'gitone.showOutput',
                    title: '$(error) GitOne unavailable',
                    tooltip: message,
                },
            ];
            if (message !== this.errorSignature) {
                this.errorSignature = message;
                this.output.appendLine(
                    `[${new Date().toISOString()}] ${this.label}: ${message}`,
                );
                void this.showFailure(
                    `GitOne could not refresh ${this.label}: ${message}`,
                );
            }
        }
    }

    private applyStatus(status: ProjectStatus): void {
        const active = new Set<string>();
        this.workingPaths.clear();
        this.stagedCount = 0;

        for (const mapped of groupChanges(status)) {
            active.add(mapped.id);
            let group = this.groups.get(mapped.id);
            if (!group) {
                group = this.sourceControl.createResourceGroup(
                    mapped.id,
                    mapped.label,
                );
                group.hideWhenEmpty = true;
                this.groups.set(mapped.id, group);
            }
            group.resourceStates = mapped.changes.map((change) =>
                this.resource(change, mapped.state),
            );
            if (mapped.state === 'staged') {
                this.stagedCount += mapped.changes.length;
            } else {
                for (const change of mapped.changes) {
                    this.workingPaths.set(
                        path.normalize(path.join(this.root, change.path)),
                        change.repository,
                    );
                }
            }
        }

        if (status.issues.length > 0) {
            active.add('issues');
            let group = this.groups.get('issues');
            if (!group) {
                group = this.sourceControl.createResourceGroup(
                    'issues',
                    'GitOne Issues',
                );
                group.hideWhenEmpty = true;
                this.groups.set('issues', group);
            }
            group.resourceStates = status.issues.map((issue) =>
                this.issueResource(issue),
            );
        }

        for (const [id, group] of this.groups) {
            if (!active.has(id)) {
                group.dispose();
                this.groups.delete(id);
            }
        }

        this.sourceControl.count = status.changes.length + status.issues.length;
        this.sourceControl.statusBarCommands = statusBarCommands(
            status,
            this.root,
        );
        this.sourceControl.inputBox.placeholder = inputBoxPlaceholder(status);
        this.updateFileDecorations(status);
        this.documents.invalidate(this.root);
        this.reportIssues(status.issues);
    }

    private resource(
        change: ChangeStatus,
        state: 'staged' | 'working',
    ): GitOneResource {
        const resourceUri = vscode.Uri.file(path.join(this.root, change.path));
        const previousPath = change.from ?? change.path;
        const left = this.documents.create({
            root: this.root,
            repository: change.repository,
            source: state === 'staged' ? 'head' : 'index',
            path: previousPath,
        });
        const right =
            state === 'staged'
                ? this.documents.create({
                      root: this.root,
                      repository: change.repository,
                      source: 'index',
                      path: change.path,
                  })
                : change.type === 'deleted'
                  ? this.documents.create({
                        root: this.root,
                        repository: change.repository,
                        source: 'empty',
                        path: change.path,
                    })
                  : resourceUri;
        const renamed = change.from ? ` from ${change.from}` : '';
        return {
            resourceUri,
            projectRoot: this.root,
            repository: change.repository,
            state,
            path: change.path,
            openUri: change.type === 'deleted' ? left : resourceUri,
            contextValue: `${state}:${change.type}`,
            command: {
                command: 'vscode.diff',
                title: 'Open Change',
                arguments: [
                    left,
                    right,
                    `${change.path} (${change.repository}: ${change.type}${renamed})`,
                ],
            },
            decorations: {
                tooltip: `${change.repository}: ${change.type}${renamed}`,
                strikeThrough: change.type === 'deleted',
            },
        };
    }

    private updateFileDecorations(reported: ProjectStatus): void {
        const changes = reported.changes;
        const next = new Map<string, vscode.FileDecoration>();
        // A path can be staged and changed again in the working tree, but VS Code
        // shows one decoration per file. Staged decorations are written first, so
        // the working-tree decoration overwrites them and the newer state wins.
        for (const state of ['staged', 'working'] as const) {
            for (const change of changes) {
                if (
                    (change.state === 'staged' ? 'staged' : 'working') !== state
                ) {
                    continue;
                }
                const status = statusDecoration(change);
                const decoration = new vscode.FileDecoration(
                    status.badge,
                    status.tooltip,
                    new vscode.ThemeColor(status.color),
                );
                decoration.propagate = status.propagate;
                next.set(
                    vscode.Uri.file(
                        path.join(this.root, change.path),
                    ).toString(),
                    decoration,
                );
            }
        }
        const changed = new Set([
            ...this.fileDecorations.keys(),
            ...next.keys(),
        ]);
        this.fileDecorations = next;
        const ignoreChanged =
            !samePaths(this.ignored, reported.ignored) ||
            !samePaths(this.ignoreExceptions, reported.ignoreExceptions);
        this.ignored = reported.ignored;
        this.ignoreExceptions = reported.ignoreExceptions;
        if (ignoreChanged) {
            // One entry can stand for a whole directory tree, so the affected
            // paths are not enumerable. undefined refreshes every decoration.
            this.fileDecorationEmitter.fire(undefined);
            return;
        }
        this.fileDecorationEmitter.fire(
            [...changed].map((value) => vscode.Uri.parse(value, true)),
        );
    }

    private issueResource(issue: ProjectIssue): GitOneResource {
        const resourceUri = vscode.Uri.file(
            path.join(this.root, issue.path || '.'),
        );
        return {
            resourceUri,
            projectRoot: this.root,
            repository: '',
            state: 'working',
            path: issue.path,
            openUri: resourceUri,
            contextValue: 'issue',
            command: {
                command: 'gitone.showOutput',
                title: 'Show GitOne Output',
            },
            decorations: {
                iconPath: new vscode.ThemeIcon('warning'),
                tooltip: `${issue.code}: ${issue.detail}`,
            },
        };
    }

    private originalResource(uri: vscode.Uri): vscode.Uri | undefined {
        const repository = this.workingPaths.get(path.normalize(uri.fsPath));
        const relative = this.projectPath(uri);
        if (!repository || relative === undefined) {
            return undefined;
        }
        return this.documents.create({
            root: this.root,
            repository,
            source: 'index',
            path: relative,
        });
    }

    // projectPath is the project-relative path of a file below this project,
    // and undefined for the project root itself and anything outside it.
    private projectPath(uri: vscode.Uri): string | undefined {
        if (uri.scheme !== 'file') {
            return undefined;
        }
        const relative = path
            .relative(this.root, path.normalize(uri.fsPath))
            .split(path.sep)
            .join('/');
        if (
            relative === '' ||
            relative === '..' ||
            relative.startsWith('../') ||
            path.isAbsolute(relative)
        ) {
            return undefined;
        }
        return relative;
    }

    private fileChanged(uri: vscode.Uri): void {
        const relative = this.projectPath(uri);
        if (
            relative === undefined ||
            relative === '.gitone' ||
            relative.startsWith('.gitone/')
        ) {
            return;
        }
        this.scheduleRefresh();
    }

    private runMutation(
        args: readonly string[],
        input?: string,
    ): Promise<boolean> {
        let succeeded = false;
        const operation = this.mutation
            .catch(() => undefined)
            .then(async () => {
                this.busy = true;
                try {
                    await this.cli.run(args, { input, waitForLock: true });
                    succeeded = true;
                } catch (error) {
                    if (!(error instanceof vscode.CancellationError)) {
                        await this.showFailure(errorMessage(error));
                    }
                } finally {
                    this.busy = false;
                    this.refreshRequested = true;
                    await this.refresh();
                }
            });
        this.mutation = operation;
        return operation.then(() => succeeded);
    }

    private reportIssues(issues: readonly ProjectIssue[]): void {
        const signature = JSON.stringify(issues);
        if (signature === this.issueSignature) {
            return;
        }
        this.issueSignature = signature;
        if (issues.length === 0) {
            return;
        }
        for (const issue of issues) {
            this.output.appendLine(
                `${issue.code} ${issue.path}: ${issue.detail}`,
            );
        }
        void vscode.window
            .showWarningMessage(
                `GitOne reports ${issues.length} unsafe project ${issues.length === 1 ? 'issue' : 'issues'} in ${this.label}.`,
                'Show Output',
            )
            .then((answer) => {
                if (answer === 'Show Output') {
                    this.output.show(true);
                }
            });
    }

    private async showFailure(message: string): Promise<void> {
        const answer = await vscode.window.showErrorMessage(
            message,
            'Open Terminal',
            'Show Output',
        );
        if (answer === 'Open Terminal') {
            vscode.window
                .createTerminal({
                    name: `GitOne: ${this.label}`,
                    cwd: this.root,
                })
                .show();
        } else if (answer === 'Show Output') {
            this.output.show(true);
        }
    }
}

function selectedPaths(
    resources: readonly GitOneResource[],
    root: string,
    state: GitOneResource['state'],
): string[] {
    return [
        ...new Set(
            resources
                .filter(
                    (resource) =>
                        resource.projectRoot === root &&
                        resource.state === state &&
                        resource.path !== '',
                )
                .map((resource) => resource.path),
        ),
    ];
}

function cliPath(value: string): string {
    return value.startsWith('-') ? `./${value}` : value;
}

function statusBarCommands(
    status: ProjectStatus,
    root: string,
): vscode.Command[] {
    const branches = new Set(
        status.repositories.map((repository) => repository.branch),
    );
    const branch =
        branches.size === 1
            ? [...branches][0]
            : `${status.repositories.length} repositories`;
    const commands: vscode.Command[] = [
        {
            command: 'gitone.refresh',
            title: `$(git-branch) ${branch ?? 'GitOne'}`,
            tooltip: 'Refresh GitOne',
            arguments: [root],
        },
    ];
    if (status.issues.length > 0) {
        commands.push({
            command: 'gitone.showOutput',
            title: `$(warning) ${status.issues.length}`,
            tooltip: 'Show GitOne issues',
        });
    }
    return commands;
}

// ignoredDecoration matches the built-in Git extension: the native ignored
// color and no badge.
const ignoredDecoration = new vscode.FileDecoration(
    undefined,
    undefined,
    new vscode.ThemeColor(ignoredColor),
);

function samePaths(left: readonly string[], right: readonly string[]): boolean {
    return (
        left.length === right.length &&
        left.every((entry, index) => entry === right[index])
    );
}

// commitPlaceholder is VS Code's native commit input placeholder. It is used
// unchanged whenever the project has no single branch to name.
const commitPlaceholder = 'Message ({0} to commit)';

function inputBoxPlaceholder(status: ProjectStatus): string {
    const branches = new Set(
        status.repositories.map((repository) => repository.branch),
    );
    return branches.size === 1
        ? `Message ({0} to commit on "${[...branches][0]}")`
        : commitPlaceholder;
}
