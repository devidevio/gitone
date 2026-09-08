export type ChangeState = 'staged' | 'unstaged' | 'untracked';

export interface RepositoryStatus {
    name: string;
    visibility: 'public' | 'private';
    branch: string;
    ahead: number | null;
    behind: number | null;
}

export interface ChangeStatus {
    repository: string;
    state: ChangeState;
    type:
        | 'modified'
        | 'typechanged'
        | 'added'
        | 'deleted'
        | 'renamed'
        | 'copied'
        | 'unmerged';
    path: string;
    from?: string;
}

export interface ProjectIssue {
    code: string;
    path: string;
    detail: string;
}

export interface ProjectStatus {
    version: 1;
    repositories: RepositoryStatus[];
    changes: ChangeStatus[];
    issues: ProjectIssue[];
    // ignored holds the project-relative paths the GitOne CLI evaluated with
    // native Git. An entry ending in a slash is a directory and stands for
    // everything below it except exact paths in ignoreExceptions.
    ignored: string[];
    ignoreExceptions: string[];
}

export interface ChangeGroup {
    id: string;
    label: string;
    repository: string;
    state: 'staged' | 'working';
    changes: ChangeStatus[];
}

export interface StatusDecoration {
    badge: 'M' | 'A' | 'U' | 'D' | 'R' | 'C' | 'T' | '!';
    color: string;
    tooltip: string;
    propagate: boolean;
}

const changeStates = new Set<ChangeState>(['staged', 'unstaged', 'untracked']);
const changeTypes = new Set<ChangeStatus['type']>([
    'modified',
    'typechanged',
    'added',
    'deleted',
    'renamed',
    'copied',
    'unmerged',
]);

export function parseStatus(json: string): ProjectStatus {
    const value: unknown = JSON.parse(json);
    const document = record(value, 'status');
    if (document.version !== 1) {
        throw new Error(
            `Unsupported GitOne status version: ${String(document.version)}`,
        );
    }
    const status: ProjectStatus = {
        version: 1,
        repositories: array(document.repositories, 'repositories').map(
            parseRepository,
        ),
        changes: array(document.changes, 'changes').map(parseChange),
        issues: array(document.issues, 'issues').map(parseIssue),
        ignored: contractPaths(document.ignored, 'ignored paths'),
        ignoreExceptions: contractPaths(
            document.ignore_exceptions,
            'ignore exceptions',
        ),
    };
    const repositories = new Set(
        status.repositories.map((repository) => repository.name),
    );
    if (repositories.size !== status.repositories.length) {
        throw new Error('GitOne status contains a duplicate repository.');
    }
    if (status.changes.some((change) => !repositories.has(change.repository))) {
        throw new Error(
            'GitOne status contains a change for an unknown repository.',
        );
    }
    return status;
}

export function groupChanges(status: ProjectStatus): ChangeGroup[] {
    const groups: ChangeGroup[] = [];
    for (const repository of status.repositories) {
        const staged = status.changes.filter(
            (change) =>
                change.repository === repository.name &&
                change.state === 'staged',
        );
        const working = status.changes.filter(
            (change) =>
                change.repository === repository.name &&
                change.state !== 'staged',
        );
        if (staged.length > 0) {
            groups.push({
                id: `staged:${repository.name}`,
                label: `${repository.name} - Staged Changes`,
                repository: repository.name,
                state: 'staged',
                changes: staged,
            });
        }
        if (working.length > 0) {
            groups.push({
                id: `working:${repository.name}`,
                label: `${repository.name} - Changes`,
                repository: repository.name,
                state: 'working',
                changes: working,
            });
        }
    }
    return groups;
}

export function statusDecoration(change: ChangeStatus): StatusDecoration {
    if (change.state === 'untracked') {
        return decoration(
            'U',
            'gitDecoration.untrackedResourceForeground',
            'Untracked',
        );
    }
    switch (change.type) {
        case 'added':
            return decoration(
                'A',
                'gitDecoration.addedResourceForeground',
                change.state === 'staged' ? 'Index Added' : 'Added',
            );
        case 'deleted':
            return decoration(
                'D',
                change.state === 'staged'
                    ? 'gitDecoration.stageDeletedResourceForeground'
                    : 'gitDecoration.deletedResourceForeground',
                change.state === 'staged' ? 'Index Deleted' : 'Deleted',
                false,
            );
        case 'renamed':
            return decoration(
                'R',
                'gitDecoration.renamedResourceForeground',
                change.state === 'staged' ? 'Index Renamed' : 'Renamed',
            );
        case 'copied':
            return decoration(
                'C',
                'gitDecoration.renamedResourceForeground',
                change.state === 'staged' ? 'Index Copied' : 'Copied',
            );
        case 'typechanged':
            return decoration(
                'T',
                'gitDecoration.modifiedResourceForeground',
                'Type Changed',
            );
        case 'unmerged':
            return decoration(
                '!',
                'gitDecoration.conflictingResourceForeground',
                'Conflict',
            );
        default:
            return decoration(
                'M',
                change.state === 'staged'
                    ? 'gitDecoration.stageModifiedResourceForeground'
                    : 'gitDecoration.modifiedResourceForeground',
                change.state === 'staged' ? 'Index Modified' : 'Modified',
            );
    }
}

// contractPaths reads a required path listing. CLI and extension are installed
// separately, so the message names the fix instead of reporting a generic
// parse failure.
function contractPaths(value: unknown, name: string): string[] {
    if (!Array.isArray(value)) {
        throw new Error(
            `This GitOne CLI does not report ${name}. Update the GitOne CLI to match the extension.`,
        );
    }
    return value.map((entry) => text(entry, name));
}

// ignoredPath reports whether a project-relative path is hidden by the ignore
// rules the CLI evaluated. A directory entry hides everything below it, which
// is exactly Git's own rule: a path below an excluded directory cannot be
// re-included. The listing is collapsed, so the scan stays short.
export function ignoredPath(
    ignored: readonly string[],
    exceptions: readonly string[],
    relativePath: string,
): boolean {
    if (exceptions.includes(relativePath)) {
        return false;
    }
    return ignored.some((entry) =>
        entry.endsWith('/')
            ? relativePath === entry.slice(0, -1) ||
              relativePath.startsWith(entry)
            : relativePath === entry,
    );
}

// ignoredColor is VS Code's native ignored-resource color. An ignored path
// gets no badge, like the built-in Git extension.
export const ignoredColor = 'gitDecoration.ignoredResourceForeground';

function decoration(
    badge: StatusDecoration['badge'],
    color: string,
    tooltip: string,
    propagate = true,
): StatusDecoration {
    return { badge, color, tooltip, propagate };
}

function parseRepository(value: unknown): RepositoryStatus {
    const item = record(value, 'repository');
    const visibility = text(item.visibility, 'repository.visibility');
    if (visibility !== 'public' && visibility !== 'private') {
        throw new Error(`Invalid repository visibility: ${visibility}`);
    }
    return {
        name: text(item.name, 'repository.name'),
        visibility,
        branch: text(item.branch, 'repository.branch'),
        ahead: nullableNumber(item.ahead, 'repository.ahead'),
        behind: nullableNumber(item.behind, 'repository.behind'),
    };
}

function parseChange(value: unknown): ChangeStatus {
    const item = record(value, 'change');
    const state = text(item.state, 'change.state') as ChangeState;
    const type = text(item.type, 'change.type') as ChangeStatus['type'];
    if (!changeStates.has(state)) {
        throw new Error(`Invalid change state: ${state}`);
    }
    if (!changeTypes.has(type)) {
        throw new Error(`Invalid change type: ${type}`);
    }
    const change: ChangeStatus = {
        repository: text(item.repository, 'change.repository'),
        state,
        type,
        path: text(item.path, 'change.path'),
    };
    if (item.from !== undefined) {
        change.from = text(item.from, 'change.from');
    }
    return change;
}

function parseIssue(value: unknown): ProjectIssue {
    const item = record(value, 'issue');
    return {
        code: text(item.code, 'issue.code'),
        path: text(item.path, 'issue.path'),
        detail: text(item.detail, 'issue.detail'),
    };
}

function record(value: unknown, name: string): Record<string, unknown> {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) {
        throw new Error(`Invalid GitOne ${name}`);
    }
    return value as Record<string, unknown>;
}

function array(value: unknown, name: string): unknown[] {
    if (!Array.isArray(value)) {
        throw new Error(`Invalid GitOne ${name}`);
    }
    return value;
}

function text(value: unknown, name: string): string {
    if (typeof value !== 'string') {
        throw new Error(`Invalid GitOne ${name}`);
    }
    return value;
}

function nullableNumber(value: unknown, name: string): number | null {
    if (value === null) {
        return null;
    }
    if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) {
        throw new Error(`Invalid GitOne ${name}`);
    }
    return value;
}
