package indexer

import (
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

func componentWorkspace(project Project, declared []projectconfig.Component, paths []string) (graph.ParseResult, []graph.Diagnostic) {
	result := graph.ParseResult{Nodes: []graph.Node{{
		ID: project.ID, Kind: graph.KindRepository, Name: project.Name,
		QualifiedName: project.Name, OwnerFile: workspaceOwner,
		Properties: map[string]string{"root": project.Root, "branch": project.Branch},
	}}}
	components := append([]projectconfig.Component(nil), declared...)
	sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })
	eligible := append([]string(nil), paths...)
	sort.Strings(eligible)
	var diagnostics []graph.Diagnostic
	for _, component := range components {
		componentID := graph.NodeID(graph.KindComponent, project.ID+":"+component.Name)
		componentLocation := graph.Location{Path: projectconfig.FileName, Line: component.Line, Column: component.Column}
		result.Nodes = append(result.Nodes, graph.Node{
			ID: componentID, Kind: graph.KindComponent, Name: component.Name,
			QualifiedName: project.Name + "/" + component.Name,
			Location:      componentLocation, OwnerFile: workspaceOwner,
		})
		result.Facts = append(result.Facts, graph.Fact{
			ID:     stableWorkspaceFactID(project.ID, graph.EdgeContains, componentID),
			FromID: project.ID, Kind: graph.EdgeContains, TargetID: componentID,
			Location: componentLocation, OwnerFile: workspaceOwner,
		})
		roots := append([]projectconfig.ComponentRoot(nil), component.Roots...)
		sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
		matched := 0
		for _, filePath := range eligible {
			root, ok := matchingComponentRoot(roots, filePath)
			if !ok {
				continue
			}
			matched++
			fileID := graph.NodeID(graph.KindFile, project.ID+":"+filePath)
			result.Facts = append(result.Facts, graph.Fact{
				ID:     stableWorkspaceFactID(componentID, graph.EdgeContains, fileID),
				FromID: componentID, Kind: graph.EdgeContains, TargetID: fileID,
				Location:   graph.Location{Path: projectconfig.FileName, Line: root.Line, Column: root.Column},
				Properties: map[string]string{"root": root.Path}, OwnerFile: workspaceOwner,
			})
		}
		if matched == 0 {
			diagnostics = append(diagnostics, graph.Diagnostic{
				Path: projectconfig.FileName, Line: component.Line, Level: "warning",
				Message: "component \"" + component.Name + "\" matches no indexed files",
			})
		}
	}
	return result, diagnostics
}

func matchingComponentRoot(roots []projectconfig.ComponentRoot, filePath string) (projectconfig.ComponentRoot, bool) {
	for _, root := range roots {
		if root.Path == "." || filePath == root.Path || strings.HasPrefix(filePath, root.Path+"/") {
			return root, true
		}
	}
	return projectconfig.ComponentRoot{}, false
}

func stableWorkspaceFactID(from string, kind graph.EdgeKind, target string) string {
	return graph.StableID("f", workspaceOwner, from, string(kind), target)
}
