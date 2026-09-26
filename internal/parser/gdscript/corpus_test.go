package gdscript_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
)

func TestCorpus(t *testing.T) {
	root := os.Getenv("GRAFO_GDSCRIPT_CORPUS")
	if root == "" {
		t.Skip("set GRAFO_GDSCRIPT_CORPUS to a representative GDScript project")
	}
	parser := gdscriptparser.New()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".gd") {
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
		t.Fatal("corpus contains no .gd files")
	}
}
