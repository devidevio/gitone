import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { createRequire } from 'node:module';
import { PassThrough } from 'node:stream';
import test from 'node:test';

import type * as Cli from '../cli';
import type * as Provider from '../provider';

const lockMessage =
    'LOCK001 another GitOne operation is currently running: PID 123, gitone push';

interface Call {
    cwd: string;
    args: string[];
    finish: (code?: number, stdout?: string, stderr?: string) => void;
}

let calls: Call[] = [];
let logs: string[] = [];
let errors: string[] = [];
let elapsed = 0;
let notifications = 0;
let cancelled = false;
let onDelay: () => void = () => {};

// The extension waits on the clock and on child processes; both answer
// instantly here, so a 30 second wait costs no real time.
performance.now = () => elapsed;

const disposable = { dispose() {} };
const subscribe = () => disposable;

function spawn(
    _executable: string,
    args: string[],
    options: { cwd: string },
): EventEmitter {
    const child = Object.assign(new EventEmitter(), {
        stdout: new PassThrough(),
        stderr: new PassThrough(),
        stdin: new PassThrough(),
    });
    calls.push({
        cwd: options.cwd,
        args,
        finish(code = 0, stdout = '', stderr = '') {
            if (stdout) child.stdout.write(stdout);
            if (stderr) child.stderr.write(stderr);
            child.emit('close', code);
        },
    });
    return child;
}

const vscode = {
    CancellationError: class CancellationError extends Error {},
    EventEmitter: class {
        readonly event = subscribe;
        fire() {}
        dispose() {}
    },
    FileDecoration: class {},
    ThemeColor: class {},
    ThemeIcon: class {},
    RelativePattern: class {},
    Uri: { file: (value: string) => ({ fsPath: value }) },
    ProgressLocation: { Notification: 15 },
    scm: {
        createSourceControl: () => ({
            inputBox: { placeholder: '', value: '' },
            createResourceGroup: () => ({
                resourceStates: [],
                dispose() {},
            }),
            dispose() {},
        }),
    },
    window: {
        registerFileDecorationProvider: subscribe,
        showErrorMessage: (message: string) => {
            errors.push(message);
            return Promise.resolve(undefined);
        },
        showWarningMessage: () => Promise.resolve(undefined),
        withProgress: (
            _options: unknown,
            action: (progress: unknown, token: unknown) => Promise<unknown>,
        ) => {
            notifications++;
            return action(
                { report() {} },
                {
                    get isCancellationRequested() {
                        return cancelled;
                    },
                },
            );
        },
    },
    workspace: {
        createFileSystemWatcher: () => ({
            onDidCreate: subscribe,
            onDidChange: subscribe,
            onDidDelete: subscribe,
            dispose() {},
        }),
    },
};

// Replace the editor and process boundaries for every compiled module below,
// then load the extension the way Node loads it in production.
interface ModuleLoader {
    _load(request: string, parent: unknown, isMain: boolean): unknown;
}

const nativeRequire = createRequire(__filename);
const stubs = new Map<string, unknown>([
    ['vscode', vscode],
    ['node:child_process', { spawn }],
    [
        'node:timers/promises',
        {
            setTimeout: (milliseconds: number) => {
                elapsed += milliseconds;
                onDelay();
                return Promise.resolve();
            },
        },
    ],
]);
const loader = nativeRequire('node:module') as ModuleLoader;
const loadModule = loader._load;
loader._load = (request, parent, isMain) =>
    stubs.get(request) ?? loadModule.call(loader, request, parent, isMain);

const cli = nativeRequire('../cli') as typeof Cli;
const { GitOneProvider } = nativeRequire('../provider') as typeof Provider;

const emptyStatus = JSON.stringify({
    version: 1,
    repositories: [],
    changes: [],
    issues: [],
    ignored: [],
    ignore_exceptions: [],
});

const channel = { appendLine: (line: string) => logs.push(line) } as never;

function harness() {
    calls = [];
    logs = [];
    errors = [];
    elapsed = 0;
    notifications = 0;
    cancelled = false;
    onDelay = () => {};
    return {
        calls,
        logs,
        errors,
        client: (root: string) => new cli.GitOneCli('gitone', root, channel),
        provider: (root: string) =>
            new GitOneProvider(
                root,
                'a',
                new cli.GitOneCli('gitone', root, channel),
                { invalidate() {} } as never,
                channel,
            ),
        onDelay: (action: () => void) => {
            onDelay = action;
        },
        cancel: () => {
            cancelled = true;
        },
        state: () => ({ elapsed, notifications }),
    };
}

async function flush() {
    await new Promise<void>((resolve) => setImmediate(resolve));
}

// refuseUntilTimeout answers every dispatched call with a lock refusal until
// the client stops retrying.
async function refuseUntilTimeout(): Promise<void> {
    for (let attempt = 0; ; attempt++) {
        calls[attempt].finish(1, '', lockMessage);
        await flush();
        if (calls.length === attempt + 1) {
            return;
        }
    }
}

// refreshWith drives one status load and answers the CLI call it dispatches.
async function refreshWith(
    provider: Provider.GitOneProvider,
    code: number,
    stdout: string,
    stderr: string,
): Promise<void> {
    const index = calls.length;
    const refresh = provider.refresh();
    await flush();
    calls[index].finish(code, stdout, stderr);
    await refresh;
}

test('serializes status/show/mutations per project while other projects proceed', async () => {
    const h = harness();
    const a = h.client('/projects/a');
    const b = h.client('/projects/b');
    const status = a.run(['status', '--json']);
    const show = a.run(['show']);
    const add = a.run(['add', 'file.txt']);
    const other = b.run(['push']);
    await flush();
    assert.deepEqual(
        h.calls.map(({ cwd, args }) => [cwd, args[0]]),
        [
            ['/projects/a', 'status'],
            ['/projects/b', 'push'],
        ],
    );
    h.calls[1].finish();
    await other;
    h.calls[0].finish();
    await status;
    await flush();
    assert.equal(h.calls[2].args[0], 'show');
    h.calls[2].finish();
    await show;
    await flush();
    assert.equal(h.calls[3].args[0], 'add');
    h.calls[3].finish();
    await add;
});

test('accepted status exit 1 still rejects LOCK001 and logs project/owner', async () => {
    const h = harness();
    const client = h.client('/projects/example');
    const status = client.run(['status', '--json'], {
        acceptedExitCodes: [0, 1],
    });
    const rejected = assert.rejects(status, cli.isLockError);
    await flush();
    h.calls[0].finish(1, '', lockMessage);
    await rejected;
    assert.match(
        h.logs[0],
        /\/projects\/example: gitone status exited 1: LOCK001.*PID 123/,
    );
    const issues = client.run(['status', '--json'], {
        acceptedExitCodes: [0, 1],
    });
    await flush();
    h.calls[1].finish(1, '{"issues":[{"code":"PATH001"}]}');
    assert.equal((await issues).exitCode, 1);
});

test('lock refusal retries in place, keeping later calls queued', async () => {
    const h = harness();
    const client = h.client('/projects/a');
    const operation = client.run(['add', 'file.txt'], { waitForLock: true });
    const following = client.run(['show']);
    await flush();
    h.calls[0].finish(1, '', lockMessage);
    await flush();
    assert.deepEqual(
        h.calls.map(({ args }) => args[0]),
        ['add', 'add'],
    );
    h.calls[1].finish();
    await operation;
    await flush();
    h.calls[2].finish();
    await following;
    assert.equal(h.state().notifications, 1);
});

test('cancelled waiting dispatches no retry and releases the queue', async () => {
    const h = harness();
    h.onDelay(h.cancel);
    const client = h.client('/projects/a');
    const operation = client.run(['add'], { waitForLock: true });
    const rejected = assert.rejects(operation, vscode.CancellationError);
    await flush();
    h.calls[0].finish(1, '', lockMessage);
    await rejected;
    assert.equal(h.calls.length, 1);
    const next = client.run(['status']);
    await flush();
    h.calls[1].finish();
    await next;
});

test('waiting backs off, expires at 30 seconds and logs one refusal', async () => {
    const h = harness();
    const operation = h.client('/projects/a').run(['show'], {
        waitForLock: true,
    });
    const rejected = assert.rejects(
        operation,
        /Timed out waiting.*\/projects\/a.*LOCK001/,
    );
    await flush();
    await refuseUntilTimeout();
    await rejected;
    assert.equal(h.state().elapsed, 30_000);
    // 250 ms doubling to 1 s: 33 dispatches over the budget, not 120.
    assert.equal(h.calls.length, 33);
    assert.equal(h.logs.filter((line) => line.includes('LOCK001')).length, 1);
});

test('queued calls share one waiting budget', async () => {
    const h = harness();
    const client = h.client('/projects/a');
    const first = client.run(['show'], { waitForLock: true });
    const second = client.run(['add'], { waitForLock: true });
    const rejected = Promise.all([
        assert.rejects(first, /Timed out waiting/),
        assert.rejects(second, /Timed out waiting/),
    ]);
    await flush();
    await refuseUntilTimeout();
    await rejected;
    assert.equal(h.state().elapsed, 30_000);
});

test('other errors and failures with output are never retried', async () => {
    for (const [stdout, stderr] of [
        ['', 'REC001 recovery required'],
        ['Already wrote data', lockMessage],
    ]) {
        const h = harness();
        const operation = h
            .client('/projects/a')
            .run(['push'], { waitForLock: true });
        const rejected = assert.rejects(operation);
        await flush();
        h.calls[0].finish(1, stdout, stderr);
        await rejected;
        assert.equal(h.calls.length, 1);
        assert.equal(h.state().notifications, 0);
    }
});

test('provider preserves status on lock refusal and retries once', async () => {
    const h = harness();
    const provider = h.provider('/projects/a');
    const retries: number[] = [];
    provider.scheduleRefresh = (delay = 250) => {
        retries.push(delay);
    };
    const statusBarCommands = provider.sourceControl.statusBarCommands;

    await refreshWith(provider, 1, '', lockMessage);
    assert.equal(provider.sourceControl.statusBarCommands, statusBarCommands);
    assert.deepEqual(h.errors, []);
    assert.deepEqual(retries, [1_000]);

    // A lock that is still held must not start a second timer chain.
    await refreshWith(provider, 1, '', lockMessage);
    assert.deepEqual(retries, [1_000]);

    // A released lock ends the contention, so the next one may retry again.
    await refreshWith(provider, 0, emptyStatus, '');
    await refreshWith(provider, 1, '', lockMessage);
    assert.deepEqual(retries, [1_000, 1_000]);
});

test('provider reports failures that are not lock refusals', async () => {
    const h = harness();
    const provider = h.provider('/projects/a');
    provider.scheduleRefresh = () => {};
    await refreshWith(provider, 1, '', 'REC001 recovery required');
    assert.equal(h.errors.length, 1);
    assert.match(h.errors[0], /REC001/);
});
