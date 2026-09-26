package godot_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
)

func TestCorpus(t *testing.T) {
	root := os.Getenv("GRAFO_GODOT_CORPUS")
	if root == "" {
		t.Skip("set GRAFO_GODOT_CORPUS to a representative Godot project")
	}
	parser := godotparser.New()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root && (entry.Name() == ".git" || entry.Name() == ".godot") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !parser.Supports(path) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, err = parser.Parse(context.Background(), parserapi.Input{
			Root: root, Path: filepath.ToSlash(relative), Content: content, RepoID: "corpus",
		})
		if err != nil {
			t.Errorf("parse %s: %v", relative, err)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("corpus contains no supported Godot files")
	}
}
