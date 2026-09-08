import { spawn } from 'node:child_process';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

import * as vscode from 'vscode';

export const VS_CODE_PROTOCOL = 1;

// One contention phase of a client may wait this long in total. Queued calls
// share the budget, so they cannot multiply the delay the user sees.
const lockTimeout = 30_000;
const firstLockDelay = 250;
const maxLockDelay = 1_000;

export interface CommandResult {
    stdout: Buffer;
    stderr: string;
    exitCode: number;
}

export interface RunOptions {
    input?: string;
    // acceptedExitCodes are the exit codes that still carry usable output.
    acceptedExitCodes?: readonly number[];
    // waitForLock retries an explicit pre-execution lock refusal.
    waitForLock?: boolean;
}

export class CliError extends Error {
    constructor(
        message: string,
        readonly exitCode?: number,
        readonly stderr = '',
        readonly lockRefused = false,
    ) {
        super(message);
        this.name = 'CliError';
    }
}

export class GitOneCli {
    private queue: Promise<unknown> = Promise.resolve();
    private pending = 0;
    // lockDeadline is shared by everything waiting in one contention phase and
    // is cleared once the client falls idle.
    private lockDeadline = 0;

    constructor(
        readonly executable: string,
        readonly cwd: string,
        private readonly output: vscode.OutputChannel,
    ) {}

    run(
        args: readonly string[],
        options: RunOptions = {},
    ): Promise<CommandResult> {
        this.pending += 1;
        const operation = this.queue.then(async () => {
            try {
                return await this.execute(args, options);
            } catch (error) {
                if (!options.waitForLock || !isLockError(error)) {
                    throw error;
                }
                return this.waitForLock(args, options, error);
            }
        });
        this.queue = operation
            .catch(() => undefined)
            .finally(() => {
                this.pending -= 1;
                if (this.pending === 0) {
                    this.lockDeadline = 0;
                }
            });
        return operation;
    }

    private waitForLock(
        args: readonly string[],
        options: RunOptions,
        initialError: CliError,
    ): Promise<CommandResult> {
        if (this.lockDeadline === 0) {
            this.lockDeadline = performance.now() + lockTimeout;
        }
        const deadline = this.lockDeadline;
        const expired = (last: CliError) =>
            new CliError(
                `Timed out waiting for GitOne in ${this.cwd}. ${last.message}`,
            );
        if (performance.now() >= deadline) {
            return Promise.reject(expired(initialError));
        }
        return Promise.resolve(
            vscode.window.withProgress(
                {
                    location: vscode.ProgressLocation.Notification,
                    title: `Waiting for GitOne ${args[0]} in ${this.cwd}`,
                    cancellable: true,
                },
                async (progress, token) => {
                    let lastError = initialError;
                    let wait = firstLockDelay;
                    let remaining = deadline - performance.now();
                    while (remaining > 0) {
                        if (token.isCancellationRequested) {
                            throw new vscode.CancellationError();
                        }
                        progress.report({
                            message: `${lastError.message}. Cancel stops waiting only.`,
                        });
                        await delay(Math.min(wait, remaining));
                        wait = Math.min(wait * 2, maxLockDelay);
                        if (token.isCancellationRequested) {
                            throw new vscode.CancellationError();
                        }
                        try {
                            // Cancellation never kills an operation that may
                            // already be writing.
                            return await this.execute(args, options, true);
                        } catch (error) {
                            if (!isLockError(error)) {
                                throw error;
                            }
                            lastError = error;
                        }
                        remaining = deadline - performance.now();
                    }
                    throw expired(lastError);
                },
            ),
        );
    }

    private execute(
        args: readonly string[],
        options: RunOptions,
        retrying = false,
    ): Promise<CommandResult> {
        const command = args[0] ?? 'unknown';
        const accepted = options.acceptedExitCodes ?? [0];
        return new Promise((resolve, reject) => {
            const child = spawn(this.executable, [...args], {
                cwd: this.cwd,
                shell: false,
                windowsHide: true,
                stdio: ['pipe', 'pipe', 'pipe'],
            });
            const stdout: Buffer[] = [];
            const stderr: Buffer[] = [];
            let settled = false;

            child.stdout.on('data', (chunk) => stdout.push(Buffer.from(chunk)));
            child.stderr.on('data', (chunk) => stderr.push(Buffer.from(chunk)));
            child.stdin.on('error', () => undefined);
            child.once('error', (error) => {
                if (settled) {
                    return;
                }
                settled = true;
                this.log(command, undefined, error.message);
                reject(
                    new CliError(
                        `Cannot run ${path.basename(this.executable)}: ${error.message}`,
                    ),
                );
            });
            child.once('close', (code) => {
                if (settled) {
                    return;
                }
                settled = true;
                const exitCode = code ?? 1;
                const errorText = Buffer.concat(stderr).toString('utf8').trim();
                const lockRefused =
                    code === 1 &&
                    stdout.length === 0 &&
                    errorText.startsWith('LOCK001 ');
                // Repeated refusals while waiting stay out of the output
                // channel; the first one and the final result are logged.
                if (!retrying || !lockRefused) {
                    this.log(command, exitCode, errorText);
                }
                // Status exit 1 can carry valid issue JSON or a pre-execution refusal.
                if (!accepted.includes(exitCode) || lockRefused) {
                    reject(
                        new CliError(
                            errorText ||
                                `GitOne ${command} failed with exit code ${exitCode}.`,
                            exitCode,
                            errorText,
                            lockRefused,
                        ),
                    );
                    return;
                }
                resolve({
                    stdout: Buffer.concat(stdout),
                    stderr: errorText,
                    exitCode,
                });
            });
            child.stdin.end(options.input);
        });
    }

    private log(
        command: string,
        exitCode: number | undefined,
        detail: string,
    ): void {
        const result =
            exitCode === undefined ? 'failed to start' : `exited ${exitCode}`;
        this.output.appendLine(
            `[${new Date().toISOString()}] ${this.cwd}: gitone ${command} ${result}${detail ? `: ${detail}` : ''}`,
        );
    }
}

export function isLockError(error: unknown): error is CliError {
    return error instanceof CliError && error.lockRefused;
}

export async function checkProtocol(cli: GitOneCli): Promise<string> {
    const result = await cli.run(['vscode', 'info', '--json']);
    let info: unknown;
    try {
        info = JSON.parse(result.stdout.toString('utf8'));
    } catch {
        throw new Error('GitOne returned invalid VS Code protocol JSON.');
    }
    if (info === null || typeof info !== 'object') {
        throw new Error(
            'GitOne returned invalid VS Code protocol information.',
        );
    }
    const document = info as Record<string, unknown>;
    if (document.protocol !== VS_CODE_PROTOCOL) {
        throw new Error(
            `Unsupported GitOne VS Code protocol: ${String(document.protocol)}.`,
        );
    }
    if (
        typeof document.gitone_version !== 'string' ||
        document.gitone_version.length === 0
    ) {
        throw new Error('GitOne did not report its version.');
    }
    return document.gitone_version;
}

// errorMessage is the text of any thrown value, so callers can report a
// failure without repeating the Error type check.
export function errorMessage(error: unknown): string {
    return error instanceof Error ? error.message : String(error);
}
