import assert from 'node:assert/strict';
import test from 'node:test';

import {
    ChangeStatus,
    groupChanges,
    ignoredPath,
    parseStatus,
    statusDecoration,
} from '../model';

test('maps duplicate paths into repository and state groups', () => {
    const status = parseStatus(
        JSON.stringify({
            version: 1,
            repositories: [
                {
                    name: 'private',
                    visibility: 'private',
                    branch: 'main',
                    ahead: null,
                    behind: null,
                },
                {
                    name: 'public',
                    visibility: 'public',
                    branch: 'main',
                    ahead: 1,
                    behind: 0,
                },
            ],
            changes: [
                {
                    repository: 'private',
                    state: 'untracked',
                    type: 'added',
                    path: 'notes/new.md',
                },
                {
                    repository: 'public',
                    state: 'staged',
                    type: 'modified',
                    path: 'README.md',
                },
                {
                    repository: 'public',
                    state: 'unstaged',
                    type: 'modified',
                    path: 'README.md',
                },
            ],
            issues: [
                {
                    code: 'PATH001',
                    path: 'stray.txt',
                    detail: 'path is not assigned',
                },
            ],
            ignored: [],
            ignore_exceptions: [],
        }),
    );

    assert.deepEqual(
        groupChanges(status).map((group) => [
            group.id,
            group.changes.map((change) => change.path),
        ]),
        [
            ['working:private', ['notes/new.md']],
            ['staged:public', ['README.md']],
            ['working:public', ['README.md']],
        ],
    );
    assert.equal(status.issues[0]?.code, 'PATH001');
});

test('rejects an incompatible status schema', () => {
    assert.throws(
        () =>
            parseStatus(
                '{"version":2,"repositories":[],"changes":[],"issues":[]}',
            ),
        /Unsupported GitOne status version/,
    );
});

test('maps GitOne changes to native Git decorations', () => {
    const base = { repository: 'app', state: 'staged' as const, path: 'file' };
    assert.deepEqual(statusDecoration({ ...base, type: 'modified' }), {
        badge: 'M',
        color: 'gitDecoration.stageModifiedResourceForeground',
        tooltip: 'Index Modified',
        propagate: true,
    });
    const decoration = (
        state: ChangeStatus['state'],
        type: ChangeStatus['type'],
    ) => {
        const result = statusDecoration({ ...base, state, type });
        return [result.badge, result.color, result.propagate];
    };
    assert.deepEqual(decoration('staged', 'added'), [
        'A',
        'gitDecoration.addedResourceForeground',
        true,
    ]);
    assert.deepEqual(decoration('untracked', 'added'), [
        'U',
        'gitDecoration.untrackedResourceForeground',
        true,
    ]);
    assert.deepEqual(decoration('staged', 'deleted'), [
        'D',
        'gitDecoration.stageDeletedResourceForeground',
        false,
    ]);
    assert.deepEqual(decoration('unstaged', 'deleted'), [
        'D',
        'gitDecoration.deletedResourceForeground',
        false,
    ]);
    assert.deepEqual(decoration('staged', 'renamed'), [
        'R',
        'gitDecoration.renamedResourceForeground',
        true,
    ]);
    assert.deepEqual(decoration('staged', 'copied'), [
        'C',
        'gitDecoration.renamedResourceForeground',
        true,
    ]);
    assert.deepEqual(decoration('unstaged', 'typechanged'), [
        'T',
        'gitDecoration.modifiedResourceForeground',
        true,
    ]);
    assert.deepEqual(decoration('unstaged', 'unmerged'), [
        '!',
        'gitDecoration.conflictingResourceForeground',
        true,
    ]);
});

test('reads the ignored listing and rejects a missing one', () => {
    const document = {
        version: 1,
        repositories: [],
        changes: [],
        issues: [],
        ignored: ['node_modules/', '.env'],
        ignore_exceptions: ['node_modules/pkg/package.json'],
    };
    assert.deepEqual(parseStatus(JSON.stringify(document)).ignored, [
        'node_modules/',
        '.env',
    ]);
    assert.deepEqual(parseStatus(JSON.stringify(document)).ignoreExceptions, [
        'node_modules/pkg/package.json',
    ]);
    assert.throws(
        () => parseStatus(JSON.stringify({ ...document, ignored: undefined })),
        /does not report ignored paths/,
    );
    assert.throws(
        () =>
            parseStatus(
                JSON.stringify({ ...document, ignore_exceptions: undefined }),
            ),
        /does not report ignore exceptions/,
    );
});

test('a directory entry hides everything below it', () => {
    const ignored = ['node_modules/', '.env', 'src/build/out.o'];
    const exceptions = ['node_modules/pkg/package.json'];
    assert.equal(ignoredPath(ignored, exceptions, 'node_modules'), true);
    assert.equal(ignoredPath(ignored, exceptions, 'node_modules/pkg'), true);
    assert.equal(
        ignoredPath(ignored, exceptions, 'node_modules/pkg/index.js'),
        true,
    );
    assert.equal(
        ignoredPath(ignored, exceptions, 'node_modules/pkg/package.json'),
        false,
    );
    assert.equal(ignoredPath(ignored, exceptions, '.env'), true);
    assert.equal(ignoredPath(ignored, exceptions, 'src/build/out.o'), true);

    // A tracked path the CLI left out stays undecorated, and a prefix that is
    // not a path boundary must not match.
    assert.equal(ignoredPath(ignored, exceptions, 'src/build/keep.txt'), false);
    assert.equal(ignoredPath(ignored, exceptions, 'node_modules.bak/x'), false);
    assert.equal(ignoredPath(ignored, exceptions, '.envrc'), false);
    assert.equal(ignoredPath(ignored, exceptions, 'src/main.go'), false);
});
