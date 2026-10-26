package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var skippedPushDirs = map[string]struct{}{
	"node_modules":  {},
	".git":          {},
	".venv":         {},
	"__pycache__":   {},
	"dist":          {},
	"build":         {},
	".next":         {},
	".nuxt":         {},
	".svelte-kit":   {},
	".turbo":        {},
	".cache":        {},
	".parcel-cache": {},
	"coverage":      {},
	"out":           {},
	"target":        {},
	".gradle":       {},
	".pytest_cache": {},
	".mypy_cache":   {},
	".tox":          {},
	"venv":          {},
}

func (c *CLI) push(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: paddock push <id> [dir] [--to <path>]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	dir, to := ".", ""
	positionals := 0
	for i := 1; i < len(args); i++ {
		if args[i] == "--to" && i+1 < len(args) {
			to = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			return fmt.Errorf("usage: paddock push <id> [dir] [--to <path>]")
		}
		if positionals > 0 {
			return fmt.Errorf("usage: paddock push <id> [dir] [--to <path>]")
		}
		dir, positionals = args[i], positionals+1
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("push source %q is not a directory", dir)
	}
	if to == "" {
		to = filepath.Join("/config", filepath.Base(filepath.Clean(root)))
	}
	if !filepath.IsAbs(to) || filepath.Clean(to) != to {
		return fmt.Errorf("--to must be an absolute clean path")
	}
	files, err := pushFiles(ctx, root)
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	var compressed int64
	archiveDone := make(chan error, 1)
	go func() {
		counter := &countWriter{writer: writer}
		gz := gzip.NewWriter(counter)
		tarWriter := tar.NewWriter(gz)
		archiveErr := writePushArchive(root, files, tarWriter)
		if closeErr := tarWriter.Close(); archiveErr == nil {
			archiveErr = closeErr
		}
		if closeErr := gz.Close(); archiveErr == nil {
			archiveErr = closeErr
		}
		compressed = counter.count
		_ = writer.CloseWithError(archiveErr)
		archiveDone <- archiveErr
	}()
	query := url.Values{"path": []string{to}}
	_, requestErr := c.client.DoReader(ctx, http.MethodPut, "/sandboxes/"+url.PathEscape(id)+"/archive?"+query.Encode(), reader, "application/gzip")
	_ = reader.Close()
	archiveErr := <-archiveDone
	if requestErr != nil {
		return requestErr
	}
	if archiveErr != nil {
		return archiveErr
	}
	_, err = fmt.Fprintf(c.Out, "Pushed %d files (%.2f MB) to %s:%s\n", len(files), float64(compressed)/(1024*1024), id, to)
	return err
}

type countWriter struct {
	writer io.Writer
	count  int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.count += int64(n)
	return n, err
}

func pushFiles(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel")
	if output, err := cmd.Output(); err == nil && strings.TrimSpace(string(output)) != "" {
		cmd = exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		data, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		var files []string
		for _, name := range strings.Split(string(data), "\x00") {
			if name != "" && !skipPushFile(name) {
				files = append(files, filepath.ToSlash(name))
			}
		}
		sort.Strings(files)
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := entry.Name()
		if entry.IsDir() {
			if _, skip := skippedPushDirs[base]; skip {
				return filepath.SkipDir
			}
		}
		if entry.IsDir() {
			return nil
		}
		if !skipPushFile(rel) {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func skipPushFile(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, "._") || base == ".DS_Store"
}

func writePushArchive(root string, files []string, tw *tar.Writer) error {
	for _, name := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if os.IsNotExist(err) {
			continue // tracked but deleted locally
		}
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if info.Mode()&os.ModeSymlink != 0 {
			header.Linkname, err = os.Readlink(full)
			if err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(full)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return nil
}
