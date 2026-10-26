// Package corpus provides safe access to the user-owned MinerU corpus.
package corpus

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var pmidPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// FileInfo describes one source file without loading its content.
type FileInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// Store safely resolves corpus documents and resources.
type Store struct {
	root      string
	realRoot  string
	mu        sync.RWMutex
	documents map[string]string
}

// New creates a corpus store and resolves the root against symlink escapes.
func New(root string) (*Store, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("evaluate root: %w", err)
	}
	canonicalRoot := filepath.Clean(realRoot)
	return &Store{root: canonicalRoot, realRoot: canonicalRoot}, nil
}

// Root returns the configured absolute corpus root.
func (s *Store) Root() string {
	return s.root
}

// ValidatePMID rejects identifiers that could be interpreted as paths.
func ValidatePMID(pmid string) error {
	if !pmidPattern.MatchString(pmid) {
		return fmt.Errorf("invalid PMID %q", pmid)
	}
	return nil
}

// Discover finds PMID directories recursively without following symlinks.
func (s *Store) Discover() ([]string, error) {
	seen := make(map[string]string)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || path == s.root || ValidatePMID(entry.Name()) != nil {
			return nil
		}
		if hasMineruFile(path) {
			seen[entry.Name()] = path
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover documents: %w", err)
	}
	pmids := make([]string, 0, len(seen))
	for pmid := range seen {
		pmids = append(pmids, pmid)
	}
	sort.Strings(pmids)
	s.mu.Lock()
	s.documents = seen
	s.mu.Unlock()
	return pmids, nil
}

// DocumentDir safely returns the directory for a discovered PMID.
func (s *Store) DocumentDir(pmid string) (string, error) {
	if err := ValidatePMID(pmid); err != nil {
		return "", err
	}
	s.mu.RLock()
	found := s.documents[pmid]
	initialized := s.documents != nil
	s.mu.RUnlock()
	if !initialized {
		if _, err := s.Discover(); err != nil {
			return "", err
		}
		s.mu.RLock()
		found = s.documents[pmid]
		s.mu.RUnlock()
	}
	if found != "" {
		return s.ensureInside(found)
	}
	return "", fs.ErrNotExist
}

// ResolveSource resolves a fixed document-relative path safely.
func (s *Store) ResolveSource(pmid, relative string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(relative)) {
		return "", errors.New("path traversal is not allowed")
	}
	dir, err := s.DocumentDir(pmid)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.FromSlash(relative))
	resolved, err := s.ensureInside(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", relative)
	}
	return resolved, nil
}

// ResolveIndexedSource safely resolves a source beneath an indexed document
// directory without rediscovering the whole corpus.
func (s *Store) ResolveIndexedSource(pmid, documentRelative, sourceRelative string) (string, error) {
	if err := ValidatePMID(pmid); err != nil {
		return "", err
	}
	for _, relative := range []string{documentRelative, sourceRelative} {
		if !filepath.IsLocal(filepath.FromSlash(relative)) {
			return "", errors.New("path traversal is not allowed")
		}
	}
	path := filepath.Join(
		s.root, filepath.FromSlash(documentRelative), filepath.FromSlash(sourceRelative),
	)
	resolved, err := s.ensureInside(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", sourceRelative)
	}
	return resolved, nil
}

// ResolveImage restricts image access to a filename inside images/.
func (s *Store) ResolveImage(pmid, image string) (string, error) {
	if image == "" || filepath.Base(image) != image || strings.ContainsAny(image, `/\\`) {
		return "", errors.New("image must be a filename, not a path")
	}
	return s.ResolveSource(pmid, filepath.ToSlash(filepath.Join("images", image)))
}

// PDF returns the PMID-named PDF or the sole PDF in the document directory.
func (s *Store) PDF(pmid string) (string, error) {
	if path, err := s.ResolveSource(pmid, pmid+".pdf"); err == nil {
		return path, nil
	}
	dir, err := s.DocumentDir(pmid)
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.pdf"))
	if err != nil || len(matches) != 1 {
		return "", fmt.Errorf("original PDF unavailable for PMID %s", pmid)
	}
	return s.ensureInside(matches[0])
}

// Inventory lists regular files under a document without following symlinks.
func (s *Store) Inventory(pmid string) ([]FileInfo, error) {
	dir, err := s.DocumentDir(pmid)
	if err != nil {
		return nil, err
	}
	files := make([]FileInfo, 0)
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		mimeType := MIMEType(path)
		files = append(files, FileInfo{
			Name:     entry.Name(),
			Path:     filepath.ToSlash(relative),
			MIMEType: mimeType,
			Size:     info.Size(),
		})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

// MIMEType returns a stable MIME type for supported corpus files.
func MIMEType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		if detected := mime.TypeByExtension(filepath.Ext(path)); detected != "" {
			return detected
		}
		return "application/octet-stream"
	}
}

func (s *Store) ensureInside(path string) (string, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(s.realRoot, realPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("resolved path escapes corpus root")
	}
	return realPath, nil
}

func hasMineruFile(path string) bool {
	for _, name := range []string{"markdown.md", "structured_content.json", "middle_json.json"} {
		if info, err := os.Stat(filepath.Join(path, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}
