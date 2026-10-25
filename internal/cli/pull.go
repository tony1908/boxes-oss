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
	"path"
	"path/filepath"
	"strings"
)

func (c *CLI) pull(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: boxes pull <id> <path> [-o dest]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	source := args[1]
	if !strings.HasPrefix(source, "/") || strings.Contains(source, "..") {
		return fmt.Errorf("path must be absolute and without traversal")
	}
	dest := path.Base(source)
	for i := 2; i < len(args); i++ {
		if (args[i] == "-o" || args[i] == "--output") && i+1 < len(args) {
			dest = args[i+1]
			i++
			continue
		}
		return fmt.Errorf("usage: boxes pull <id> <path> [-o dest]")
	}
	if err := c.fetchArtifact(ctx, id, source, dest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Out, "Downloaded %s to %s\n", source, dest)
	return err
}

// fetchArtifact downloads source from the box to dest. A gzip tar response is extracted into dest as a directory.
func (c *CLI) fetchArtifact(ctx context.Context, id, source, dest string) error {
	query := url.Values{"path": []string{source}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.client.BaseURL+"/api/v1/sandboxes/"+url.PathEscape(id)+"/artifacts?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.client.Token)
	response, err := c.client.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(response.Body)
		return &statusError{code: response.StatusCode, status: response.Status, message: strings.TrimSpace(string(data))}
	}
	if response.Header.Get("Content-Type") == "application/gzip" {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		gz, err := gzip.NewReader(response.Body)
		if err != nil {
			return err
		}
		defer gz.Close()
		return extractArtifactTar(gz, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = io.Copy(out, response.Body); err != nil {
		return err
	}
	return nil
}

func extractArtifactTar(reader io.Reader, destination string) error {
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive contains unsafe path")
		}
		name := filepath.Join(root, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tarReader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive contains unsupported entry type")
		}
	}
	return nil
}
