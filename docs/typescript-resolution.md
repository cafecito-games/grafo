# TypeScript resolution

Grafo resolves TypeScript bindings only from files tracked in the indexed repository. It does not invoke TypeScript, read `node_modules`, install packages, or access the network.

Module specifiers are resolved in this order:

1. Explicit relative paths, when the specifier includes a supported extension.
2. Extensionless relative paths using `.ts`, `.tsx`, `.d.ts`, `index.ts`, `index.tsx`, then `index.d.ts`.
3. `rootDirs` projections, provided they identify one tracked module.
4. The nearest applicable `tsconfig.json` `paths` mapping, with exact keys before wildcard keys and more-specific wildcard keys before less-specific keys. Targets retain their declared array order.
5. The effective `baseUrl`.
6. Tracked `package.json` exports, preferring `types`, `import`, `default`, then `require`, followed by tracked `types`, `module`, `main`, and package index fallbacks.

Local `extends` chains are merged from parent to child. Paths are interpreted relative to the configuration that declares them. Missing, invalid, cyclic, and non-local configuration extensions are diagnosed; config-independent relative resolution remains available.

Exports and barrel re-exports are followed to a bounded depth of 128. Cycles terminate deterministically. A name with multiple distinct reachable declarations, multiple `rootDirs` candidates, a missing export, or a missing tracked package remains unresolved. Calls through `any`, unions, intersections, dynamic property access, and unknown receivers are never assigned a concrete declaration.

Module identities are repository-relative paths without the TypeScript extension or trailing `/index`. Import/export edge properties retain the original specifier, local/imported/exported names, type-only status, and source location so canonicalization does not erase provenance.
