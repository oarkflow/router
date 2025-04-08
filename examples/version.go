package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Watch for changes in these directories/files.
var watchPaths = []string{"./configs"}

// FileVersion now stores diff information as well.
type FileVersion struct {
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
	Diff      string    `json:"diff,omitempty"`
}

// Commit represents a single commit created in Developer Mode.
type Commit struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Message   string                 `json:"message"`
	Files     map[string]FileVersion `json:"files"`
}

// VersionGroup represents a merged version (release) based on pending commits.
type VersionGroup struct {
	ID            int                    `json:"id"`
	Tag           string                 `json:"tag,omitempty"`
	CommitMessage string                 `json:"commitMessage,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Files         map[string]FileVersion `json:"files"`
}

// VersionManager manages file snapshots, pending commits, and created versions.
type VersionManager struct {
	sync.Mutex
	// Latest snapshot per file (updated via file watcher).
	latestFiles map[string]string
	// History of file changes (not directly visible to end users).
	fileVersions map[string][]FileVersion

	// Pending commits created by developers.
	commits      []Commit
	nextCommitID int

	// Merged versions (releases).
	versions  []VersionGroup
	nextVerID int

	// New: Baseline for committed files
	committedFiles map[string]string
}

func NewVersionManager() *VersionManager {
	return &VersionManager{
		latestFiles:    make(map[string]string),
		fileVersions:   make(map[string][]FileVersion),
		commits:        []Commit{},
		versions:       []VersionGroup{},
		committedFiles: make(map[string]string),
		nextCommitID:   1,
		nextVerID:      1,
	}
}

var versionManager = NewVersionManager()

// UpdateFile is called by the file watcher when a file is modified.
func (vm *VersionManager) UpdateFile(path, content string) {
	vm.Lock()
	defer vm.Unlock()
	version := FileVersion{Timestamp: time.Now(), Content: content}
	vm.latestFiles[path] = content
	vm.fileVersions[path] = append(vm.fileVersions[path], version)
	log.Printf("File updated: %s", path)
}

// formatDiff produces a git-like diff string.
func formatDiff(diffs []diffmatchpatch.Diff) string {
	var result strings.Builder
	for _, d := range diffs {
		lines := strings.Split(d.Text, "\n")
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			for _, line := range lines {
				if line != "" {
					result.WriteString(" " + line + "\n")
				}
			}
		case diffmatchpatch.DiffInsert:
			for _, line := range lines {
				if line != "" {
					result.WriteString("+" + line + "\n")
				}
			}
		case diffmatchpatch.DiffDelete:
			for _, line := range lines {
				if line != "" {
					result.WriteString("-" + line + "\n")
				}
			}
		}
	}
	return result.String()
}

// GetChanges compares the latest file snapshot against its last committed version.
func (vm *VersionManager) GetChanges() map[string]string {
	vm.Lock()
	defer vm.Unlock()
	changes := make(map[string]string)
	dmp := diffmatchpatch.New()
	// For each file tracked, use the committed baseline if available.
	for file, versions := range vm.fileVersions {
		baseline := ""
		if c, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(c)
		}
		latest := strings.TrimSpace(versions[len(versions)-1].Content)
		// Debug log to verify comparison values.
		log.Printf("Comparing file '%s': baseline='%s', latest='%s'", file, baseline, latest)
		if latest == baseline {
			continue
		}
		diffs := dmp.DiffMain(baseline, latest, false)
		dmp.DiffCleanupSemantic(diffs)
		diffText := formatDiff(diffs)
		if strings.TrimSpace(diffText) != "" {
			changes[file] = diffText
		}
	}
	return changes
}

// CreateCommit creates a new commit in Developer Mode from a selection of files.
func (vm *VersionManager) CreateCommit(selectedFiles []string, message string) Commit {
	vm.Lock()
	defer vm.Unlock()
	dmp := diffmatchpatch.New()

	commit := Commit{
		ID:        vm.nextCommitID,
		Timestamp: time.Now(),
		Message:   message,
		Files:     make(map[string]FileVersion),
	}
	for _, file := range selectedFiles {
		if versions, exists := vm.fileVersions[file]; exists && len(versions) > 0 {
			// Compute diff relative to last committed baseline.
			baseline := ""
			if c, ok := vm.committedFiles[file]; ok {
				baseline = strings.TrimSpace(c)
			}
			current := strings.TrimSpace(versions[len(versions)-1].Content)
			diffs := dmp.DiffMain(baseline, current, false)
			dmp.DiffCleanupSemantic(diffs)
			diffText := formatDiff(diffs)
			fv := FileVersion{
				Timestamp: time.Now(),
				Content:   current,
				Diff:      diffText,
			}
			commit.Files[file] = fv
			// Update committed baseline.
			vm.committedFiles[file] = current
			// Reset fileVersions for this file to only contain the committed version.
			vm.fileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: current}}
		}
	}
	vm.commits = append(vm.commits, commit)
	vm.nextCommitID++
	log.Printf("Created commit %d: %s", commit.ID, message)
	return commit
}

// MergeCommits merges all pending commits into a new version (release).
// For each file, if multiple commits modified it, all changes must be identical or a conflict is raised.
func (vm *VersionManager) MergeCommits(tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()

	mergedFiles := make(map[string]FileVersion)
	mergedMsg := []string{}
	conflicts := []string{}

	for _, commit := range vm.commits {
		mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
		for file, version := range commit.Files {
			if existing, ok := mergedFiles[file]; ok {
				if existing.Content != version.Content {
					conflicts = append(conflicts, file)
				}
			} else {
				mergedFiles[file] = version
			}
		}
	}
	if len(conflicts) > 0 {
		return VersionGroup{}, fmt.Errorf("merge conflict in files: %s", strings.Join(conflicts, ", "))
	}

	mergedVersion := VersionGroup{
		ID:            vm.nextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++

	// Clear pending commits after a successful merge.
	vm.commits = []Commit{}
	log.Printf("Created version %d with tag '%s'", mergedVersion.ID, tag)
	return mergedVersion, nil
}

// MergeSelectedCommits merges only the selected commits.
func (vm *VersionManager) MergeSelectedCommits(commitIDs []int, tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	mergedFiles := make(map[string]FileVersion)
	mergedMsg := []string{}
	conflicts := []string{}

	selectedMap := make(map[int]bool)
	for _, id := range commitIDs {
		selectedMap[id] = true
	}

	remainingCommits := []Commit{}
	for _, commit := range vm.commits {
		if selectedMap[commit.ID] {
			mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
			for file, fv := range commit.Files {
				if existing, ok := mergedFiles[file]; ok {
					if existing.Content != fv.Content {
						conflicts = append(conflicts, file)
					}
				} else {
					mergedFiles[file] = fv
				}
			}
		} else {
			remainingCommits = append(remainingCommits, commit)
		}
	}
	if len(conflicts) > 0 {
		return VersionGroup{}, fmt.Errorf("merge conflict in files: %s", strings.Join(conflicts, ", "))
	}
	mergedVersion := VersionGroup{
		ID:            vm.nextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++
	// Remove merged commits.
	vm.commits = remainingCommits
	log.Printf("Created version %d with tag '%s' merging commits: %v", mergedVersion.ID, tag, commitIDs)
	return mergedVersion, nil
}

// RevertPendingCommits clears any pending commits.
func (vm *VersionManager) RevertPendingCommits() {
	vm.Lock()
	defer vm.Unlock()
	vm.commits = []Commit{}
	log.Println("Pending commits reverted.")
}

// GetDiff returns a diff between a stored file content and a new version.
func (vm *VersionManager) GetDiff(path, newContent string) string {
	vm.Lock()
	defer vm.Unlock()
	baseline := ""
	if content, ok := vm.latestFiles[path]; ok {
		baseline = content
	}
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseline, newContent, false)
	dmp.DiffCleanupSemantic(diffs)
	return formatDiff(diffs)
}

// --- File Watcher Implementation ---

func watchFiles(paths []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	// Recursively add directories.
	for _, root := range paths {
		err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				log.Println("Watching:", path)
				return watcher.Add(path)
			}
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	// Listen for events.
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				data, err := os.ReadFile(event.Name)
				if err != nil {
					log.Printf("Error reading %s: %v", event.Name, err)
					continue
				}
				content := string(data)
				// Compute diff (for logging) using the current snapshot.
				diff := versionManager.GetDiff(event.Name, content)
				log.Printf("Change on %s\nDiff:\n%s", event.Name, diff)
				versionManager.UpdateFile(event.Name, content)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}

// --- API Handlers ---

// Returns diffs (changes) for files (used by Developer Mode).
func handleChanges(w http.ResponseWriter, r *http.Request) {
	changes := versionManager.GetChanges()
	json.NewEncoder(w).Encode(changes)
}

// Create a new commit.
type CommitPayload struct {
	Message string   `json:"message"`
	Files   []string `json:"files"`
}

func handleCommit(w http.ResponseWriter, r *http.Request) {
	var payload CommitPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	commit := versionManager.CreateCommit(payload.Files, payload.Message)
	json.NewEncoder(w).Encode(commit)
}

// List pending commits.
func handleGetCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	json.NewEncoder(w).Encode(versionManager.commits)
}

// Merge pending commits into a version.
type VersionPayload struct {
	Tag string `json:"tag"`
}

func handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	var payload VersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	ver, err := versionManager.MergeCommits(payload.Tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	json.NewEncoder(w).Encode(ver)
}

// Merge selected commits into a version.
type MergeVersionPayload struct {
	Tag       string `json:"tag"`
	CommitIDs []int  `json:"commit_ids"`
}

func handleMergeSelectedCommits(w http.ResponseWriter, r *http.Request) {
	var payload MergeVersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	ver, err := versionManager.MergeSelectedCommits(payload.CommitIDs, payload.Tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	json.NewEncoder(w).Encode(ver)
}

// Revert (clear) pending commits.
func handleRevertCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.RevertPendingCommits()
	w.Write([]byte("Pending commits reverted."))
}

// List created versions.
func handleGetVersions(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	json.NewEncoder(w).Encode(versionManager.versions)
}

// Serve the main HTML page.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

func main() {
	go watchFiles(watchPaths)

	http.HandleFunc("/api/changes", handleChanges)
	http.HandleFunc("/api/commit", handleCommit)
	http.HandleFunc("/api/commits", handleGetCommits)
	// Existing merge-all endpoint remains.
	http.HandleFunc("/api/version", handleCreateVersion)
	// New handler for merging selected commits.
	http.HandleFunc("/api/version/mergeSelected", handleMergeSelectedCommits)
	http.HandleFunc("/api/version/revert", handleRevertCommits)
	http.HandleFunc("/api/versions", handleGetVersions)
	http.HandleFunc("/", handleIndex)

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
