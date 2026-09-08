import * as vscode from 'vscode';

export type VirtualSource = 'head' | 'index' | 'empty';

export interface VirtualDocument {
    root: string;
    repository: string;
    source: VirtualSource;
    path: string;
}

export class VirtualDocuments
    implements vscode.TextDocumentContentProvider, vscode.Disposable
{
    private readonly changed = new vscode.EventEmitter<vscode.Uri>();
    private readonly requested = new Map<string, vscode.Uri>();

    readonly onDidChange = this.changed.event;

    constructor(
        private readonly read: (document: VirtualDocument) => Promise<string>,
    ) {}

    create(document: VirtualDocument): vscode.Uri {
        const query = Buffer.from(JSON.stringify(document), 'utf8').toString(
            'base64url',
        );
        return vscode.Uri.from({
            scheme: 'gitone',
            path: `/${document.path}`,
            query,
        });
    }

    async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
        const document = this.parse(uri);
        this.requested.set(uri.toString(), uri);
        if (document.source === 'empty') {
            return '';
        }
        return this.read(document);
    }

    invalidate(root: string): void {
        for (const uri of this.requested.values()) {
            if (this.parse(uri).root === root) {
                this.changed.fire(uri);
            }
        }
    }

    dispose(): void {
        this.requested.clear();
        this.changed.dispose();
    }

    private parse(uri: vscode.Uri): VirtualDocument {
        try {
            const value: unknown = JSON.parse(
                Buffer.from(uri.query, 'base64url').toString('utf8'),
            );
            if (value === null || typeof value !== 'object') {
                throw new Error();
            }
            const document = value as Record<string, unknown>;
            if (
                typeof document.root !== 'string' ||
                typeof document.repository !== 'string' ||
                typeof document.path !== 'string' ||
                (document.source !== 'head' &&
                    document.source !== 'index' &&
                    document.source !== 'empty')
            ) {
                throw new Error();
            }
            return document as unknown as VirtualDocument;
        } catch {
            throw vscode.FileSystemError.FileNotFound(uri);
        }
    }
}
