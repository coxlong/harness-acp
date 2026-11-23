package codex

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	schema "github.com/ironpark/acp-go/schema/v2"
)

type fileChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
	Kind struct {
		Type string `json:"type"`
		Move string `json:"move_path"`
	} `json:"kind"`
}

func (c fileChange) content(cwd string) (schema.ToolCallContent, schema.AbsolutePath, bool) {
	oldPath := fullPath(cwd, c.Path)
	if oldPath == "" {
		return schema.ToolCallContent{}, "", false
	}
	path := oldPath
	fileType := new(schema.DiffFileTypeText)
	var change schema.DiffChange
	switch c.Kind.Type {
	case "add":
		change = schema.NewDiffChange(schema.DiffChangeAdd{Path: path, FileType: fileType})
	case "delete":
		change = schema.NewDiffChange(schema.DiffChangeDelete{Path: path, FileType: fileType})
	default:
		if moved := fullPath(cwd, c.Kind.Move); moved != "" && moved != oldPath {
			path = moved
			change = schema.NewDiffChange(schema.DiffChangeMove{Path: path, OldPath: oldPath, FileType: fileType})
		} else {
			change = schema.NewDiffChange(schema.DiffChangeModify{Path: path, FileType: fileType})
		}
	}
	result := schema.ToolCallContentDiff{Changes: []schema.DiffChange{change}}
	if patch := c.patch(patchPath(cwd, oldPath), patchPath(cwd, path)); patch != "" {
		result.Patch = &schema.DiffPatch{Format: schema.DiffPatchFormatGitPatch, Text: patch}
	}
	return schema.NewToolCallContent(result), path, true
}

func fullPath(cwd, path string) schema.AbsolutePath {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	return schema.AbsolutePath(filepath.Clean(path))
}

func patchPath(cwd string, path schema.AbsolutePath) string {
	if rel, err := filepath.Rel(cwd, string(path)); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(rel)
	}
	return strings.TrimPrefix(filepath.ToSlash(string(path)), "/")
}

func quoteGitPath(path string) string {
	if strings.ContainsAny(path, " \"\\\n\r\t") {
		return strconv.Quote(path)
	}
	return path
}

func (c fileChange) patch(oldPath, path string) string {
	const maxPatchBytes = 256 * 1024
	if len(c.Diff) > maxPatchBytes {
		return ""
	}
	oldName, name := quoteGitPath("a/"+oldPath), quoteGitPath("b/"+path)
	header := fmt.Sprintf("diff --git %s %s\n", oldName, name)
	switch c.Kind.Type {
	case "add":
		header += "new file mode 100644\n"
		if c.Diff == "" {
			return header
		}
		return header + "--- /dev/null\n+++ " + name + "\n" + wholeFileHunk(c.Diff, true)
	case "delete":
		header += "deleted file mode 100644\n"
		if c.Diff == "" {
			return header
		}
		return header + "--- " + oldName + "\n+++ /dev/null\n" + wholeFileHunk(c.Diff, false)
	}
	_, hunks, hasHunks := strings.Cut("\n"+c.Diff, "\n@@ ")
	if oldPath != path {
		header += "rename from " + quoteGitPath(oldPath) + "\nrename to " + quoteGitPath(path) + "\n"
		if !hasHunks {
			return header
		}
	}
	if !hasHunks {
		return ""
	}
	if prefix, _, ok := strings.Cut(hunks, "\n\nMoved to: "); ok {
		hunks = prefix
	}
	return header + "--- " + oldName + "\n+++ " + name + "\n@@ " + strings.TrimRight(hunks, "\n") + "\n"
}

func wholeFileHunk(text string, added bool) string {
	body, finalNewline := strings.CutSuffix(text, "\n")
	lines := strings.Split(body, "\n")
	var out strings.Builder
	sign := "-"
	if added {
		sign = "+"
		fmt.Fprintf(&out, "@@ -0,0 +1,%d @@\n", len(lines))
	} else {
		fmt.Fprintf(&out, "@@ -1,%d +0,0 @@\n", len(lines))
	}
	for _, line := range lines {
		out.WriteString(sign)
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if !finalNewline {
		out.WriteString("\\ No newline at end of file\n")
	}
	return out.String()
}
