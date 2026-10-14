package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/lexical"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/identity"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

const lexicalVersion = "1"
const lexicalBackfillBatch = 1000

// drainLexical indexes final rows, after all batched upserts have been flushed.
// The dirty queue and postings share the caller's transaction and rollback.
func drainLexical(ctx context.Context, q *sqlcgen.Queries, limit int64) (int, error) {
	rows, err := q.LexicalDirtyNodes(ctx, limit)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if err = q.LexicalDeletePostings(ctx, row.ID); err != nil {
			return 0, err
		}
		n := nodeFromRow(row)
		if lexicalDeclaration(n) {
			for _, term := range lexical.Terms(n.Name + " " + n.QualifiedName + " " + n.Location.Path) {
				if err = q.LexicalInsertPosting(ctx, sqlcgen.LexicalInsertPostingParams{Term: term, NodeID: row.ID}); err != nil {
					return 0, err
				}
			}
		}
		if err = q.LexicalClearDirty(ctx, row.ID); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
func lexicalDeclaration(n graph.Node) bool {
	if n.External || graph.IsTestSupportNode(n) {
		return false
	}
	switch n.Kind {
	case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindClass, graph.KindInterface, graph.KindEndpoint, graph.KindEvent, graph.KindGodotInputAction, graph.KindGodotNodeGroup, graph.KindGodotAutoload, graph.KindGodotScene, graph.KindTable, graph.KindView, graph.KindConfigKey:
		return true
	}
	return false
}
func (r *Repository) backfillLexical(ctx context.Context) error {
	for {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		count, err := drainLexical(ctx, r.queries.WithTx(tx), lexicalBackfillBatch)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if count < lexicalBackfillBatch {
			break
		}
	}
	return r.SetMeta(ctx, "lexical_version", lexicalVersion)
}

func (r *ReadRepository) SearchDeclarations(ctx context.Context, q graph.DeclarationQuery) (graph.DeclarationPage, error) {
	return r.reader.SearchDeclarations(ctx, q)
}

// SearchDeclarations visits rare terms first using maintained posting counts.
// It ranks lightweight projections of all visited postings before hydrating
// the top candidates. Path/language scope precedes ranking; authoritative
// component membership is checked during bounded hydration. Exhaustion may
// therefore leave component scope incomplete, which is reported explicitly.
func (r *Repository) SearchDeclarations(ctx context.Context, request graph.DeclarationQuery) (graph.DeclarationPage, error) {
	page := graph.DeclarationPage{Matches: []graph.DeclarationMatch{}}
	if request.Limit <= 0 || request.MaxPostings <= 0 || request.MaxCandidates <= 0 || len(request.Terms) > 32 {
		return page, fmt.Errorf("declaration limits must be positive and terms at most 32")
	}
	if request.MaxMetadata < 0 {
		return page, fmt.Errorf("declaration metadata limit must not be negative")
	}
	if request.MaxMetadata == 0 {
		request.MaxMetadata = graph.DefaultDeclarationMetadata
	}
	if request.MaxMetadata < 3 {
		page.Truncated = true
		return page, nil
	}
	page.MetadataReads++
	version, err := r.Meta(ctx, "lexical_version")
	if err != nil {
		return page, err
	}
	if version != lexicalVersion {
		return page, fmt.Errorf("lexical retrieval unavailable: open index writable to backfill declaration postings")
	}
	page.MetadataReads++
	pending, err := r.queries.LexicalPending(ctx)
	if err != nil {
		return page, err
	}
	if pending {
		return page, fmt.Errorf("lexical retrieval unavailable: declaration backfill incomplete")
	}
	page.MetadataReads++
	name, err := r.repositoryName(ctx)
	if err != nil {
		return page, err
	}
	if request.Repository != "" && request.Repository != name {
		return page, nil
	}
	terms := lexical.Terms(strings.Join(request.Terms, " "))
	if len(terms) > 32 {
		return page, fmt.Errorf("at most 32 normalized declaration terms")
	}
	type termCount struct {
		term  string
		count int64
	}
	counts := []termCount{}
	for _, term := range terms {
		if page.MetadataReads >= request.MaxMetadata {
			page.Truncated = true
			break
		}
		page.MetadataReads++
		count, e := r.queries.LexicalPopulation(ctx, term)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return page, e
		}
		counts = append(counts, termCount{term, count})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].count != counts[j].count {
			return counts[i].count < counts[j].count
		}
		return counts[i].term < counts[j].term
	})
	seen := map[identity.Key]bool{}
	preliminary := []graph.DeclarationMatch{}
	for _, tc := range counts {
		remaining := request.MaxPostings - page.PostingsRead
		if remaining <= 0 {
			page.Truncated = true
			break
		}
		rows, e := r.queries.LexicalPostings(ctx, sqlcgen.LexicalPostingsParams{Term: tc.term, Limit: int64(remaining)})
		if e != nil {
			return page, e
		}
		page.PostingsRead += len(rows)
		if int64(len(rows)) < tc.count {
			page.Truncated = true
		}
		for _, row := range rows {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			n := graph.Node{ID: string(row.ID), Name: row.Name, QualifiedName: row.QualifiedName, Kind: graph.NodeKind(row.Kind), Language: row.Language, Location: graph.Location{Path: row.Path}}
			if len(request.Languages) > 0 && !slices.Contains(request.Languages, n.Language) {
				continue
			}
			if len(request.PathPrefixes) > 0 {
				matched := false
				for _, p := range request.PathPrefixes {
					p = strings.TrimSuffix(p, "/")
					if n.Location.Path == p || strings.HasPrefix(n.Location.Path, p+"/") {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			preliminary = append(preliminary, lexical.Rank(n, terms))
		}
	}
	lexical.Sort(preliminary)
	if len(preliminary) > request.MaxCandidates {
		preliminary = preliminary[:request.MaxCandidates]
		page.Truncated = true
	}
	for _, m := range preliminary {
		page.CandidatesRead++
		n, e := r.Node(ctx, m.Node.ID)
		if e != nil {
			return page, e
		}
		m.Node = n
		flags, e := r.queries.LexicalFlags(ctx, sqlcgen.LexicalFlagsParams{Path: n.Location.Path, ID: identity.Key(n.ID), Component: request.Component, ComponentID: identity.Key(request.Component)})
		if e != nil {
			return page, e
		}
		if flags.LookupTruncated.Valid && flags.LookupTruncated.Bool {
			page.Truncated = true
		}
		if !sqliteFlag(flags.InComponent) {
			continue
		}
		m.Repository = name
		m.Generated = flags.IsGenerated || n.Properties["generated_binding"] == "true"
		m.InterfaceMember = flags.InterfaceMember
		if m.Generated {
			m.Score = m.Score / 4
		}
		if m.InterfaceMember || n.Kind == graph.KindInterface {
			m.Score = m.Score / 2
		}
		page.Matches = append(page.Matches, m)
	}
	lexical.Sort(page.Matches)
	if len(page.Matches) > request.Limit {
		page.Matches = page.Matches[:request.Limit]
		page.Truncated = true
	}
	return page, nil
}

// sqliteFlag reads a SQL boolean expression that sqlc cannot type, which the
// driver may return as an integer, a boolean or their textual encodings.
func sqliteFlag(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case []byte:
		return len(typed) == 1 && typed[0] == '1'
	case string:
		return typed == "1" || typed == "true"
	default:
		return false
	}
}
