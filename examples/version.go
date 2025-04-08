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

// --- Configuration & Data Structures ---

// The watchPaths can be adjusted to point to your selected directory.
var watchPaths = []string{"./configs"}

// FileVersion stores a file snapshot and its diff.
type FileVersion struct {
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
	Diff      string    `json:"diff,omitempty"`
}

// Commit represents an individual commit with a commit message and a set of file snapshots.
type Commit struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Message   string                 `json:"message"`
	Files     map[string]FileVersion `json:"files"`
}

// VersionGroup represents a merged (released) version that is the result of merging one or more commits.
type VersionGroup struct {
	ID            int                    `json:"id"`
	Tag           string                 `json:"tag,omitempty"`
	CommitMessage string                 `json:"commitMessage,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Files         map[string]FileVersion `json:"files"`
}

// VersionManager manages file snapshots from the watcher, pending commits, and released versions.
// It also holds a baseline of committed files and a pointer to the currently deployed version.
type VersionManager struct {
	sync.Mutex
	latestFiles     map[string]string        // Latest snapshot per file (from the watcher)
	fileVersions    map[string][]FileVersion // History of snapshots per file
	commits         []Commit                 // Pending commits (created in Developer Mode)
	nextCommitID    int
	versions        []VersionGroup // Merged versions (releases)
	nextVerID       int
	committedFiles  map[string]string // Baseline contents of files after they were last committed
	deployedVersion *VersionGroup     // Currently deployed version (if any)
}

// NewVersionManager creates and initializes a new VersionManager.
func NewVersionManager() *VersionManager {
	return &VersionManager{
		latestFiles:     make(map[string]string),
		fileVersions:    make(map[string][]FileVersion),
		commits:         []Commit{},
		versions:        []VersionGroup{},
		committedFiles:  make(map[string]string),
		deployedVersion: nil,
		nextCommitID:    1,
		nextVerID:       1,
	}
}

var versionManager = NewVersionManager()

// --- Utility Functions ---

// formatDiff converts a slice of diffs into a git-like diff string.
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

// --- File Watching & Snapshot Updating ---

// UpdateFile is called by the file watcher when a file is modified.
func (vm *VersionManager) UpdateFile(path, content string) {
	vm.Lock()
	defer vm.Unlock()
	version := FileVersion{Timestamp: time.Now(), Content: content}
	vm.latestFiles[path] = content
	vm.fileVersions[path] = append(vm.fileVersions[path], version)
	log.Printf("File updated: %s", path)
}

// --- Diff & Change Detection ---

// GetChanges compares the latest snapshot against the committed baseline and returns a map of file diffs.
func (vm *VersionManager) GetChanges() map[string]string {
	vm.Lock()
	defer vm.Unlock()
	changes := make(map[string]string)
	dmp := diffmatchpatch.New()
	for file, versions := range vm.fileVersions {
		baseline := ""
		if c, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(c)
		}
		latest := strings.TrimSpace(versions[len(versions)-1].Content)
		// Log comparison details for debugging.
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

// --- Developer Mode: Commit Creation ---

// CreateCommit creates a new commit for the selected files.
// For each file, it computes the diff relative to the committed baseline,
// updates the baseline to the current state and resets the file history.
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
			// Update baseline for this file.
			vm.committedFiles[file] = current
			// Reset the file history: only store the committed version.
			vm.fileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: current}}
		}
	}
	vm.commits = append(vm.commits, commit)
	vm.nextCommitID++
	log.Printf("Created commit %d: %s", commit.ID, message)
	return commit
}

// --- Production Mode: Merging Commits into a Version ---

// MergeCommits merges all pending commits sequentially.
// For each file, the function applies changes one by one from a baseline,
// detecting conflicts if a patch fails to apply. If any file has conflicting changes,
// an error is returned. Otherwise, a new VersionGroup is created.
func (vm *VersionManager) MergeCommits(tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()

	mergedMsg := []string{}
	fileCommits := make(map[string][]FileVersion)
	var conflicts []string
	dmp := diffmatchpatch.New()

	// Gather file changes from all pending commits.
	for _, commit := range vm.commits {
		mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
		for file, version := range commit.Files {
			fileCommits[file] = append(fileCommits[file], version)
		}
	}

	mergedFiles := make(map[string]FileVersion)
	// For each file with changes, start from its baseline and apply each commit sequentially.
	for file, changes := range fileCommits {
		baseline := ""
		if b, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(b)
		}
		merged := baseline
		for _, candidate := range changes {
			patches := dmp.PatchMake(merged, strings.TrimSpace(candidate.Content))
			newMerged, results := dmp.PatchApply(patches, merged)
			// If any patch could not be applied, register a conflict.
			for _, applied := range results {
				if !applied {
					conflicts = append(conflicts, file)
					break
				}
			}
			if len(conflicts) > 0 {
				break
			}
			merged = newMerged
		}
		if len(conflicts) > 0 {
			break
		}
		finalDiff := formatDiff(dmp.DiffMain(baseline, merged, false))
		mergedFiles[file] = FileVersion{
			Timestamp: time.Now(),
			Content:   merged,
			Diff:      finalDiff,
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
	// Clear pending commits upon a successful merge.
	vm.commits = []Commit{}
	log.Printf("Created version %d with tag '%s'", mergedVersion.ID, tag)
	return mergedVersion, nil
}

// MergeSelectedCommits merges only the commits whose IDs are provided.
// Commits that are not merged remain pending.
func (vm *VersionManager) MergeSelectedCommits(commitIDs []int, tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()

	mergedMsg := []string{}
	fileCommits := make(map[string][]FileVersion)
	var conflicts []string
	dmp := diffmatchpatch.New()

	selectedMap := make(map[int]bool)
	for _, id := range commitIDs {
		selectedMap[id] = true
	}
	remainingCommits := []Commit{}

	// Gather file changes from selected commits.
	for _, commit := range vm.commits {
		if selectedMap[commit.ID] {
			mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
			for file, fv := range commit.Files {
				fileCommits[file] = append(fileCommits[file], fv)
			}
		} else {
			remainingCommits = append(remainingCommits, commit)
		}
	}

	mergedFiles := make(map[string]FileVersion)
	for file, changes := range fileCommits {
		baseline := ""
		if b, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(b)
		}
		merged := baseline
		for _, candidate := range changes {
			patches := dmp.PatchMake(merged, strings.TrimSpace(candidate.Content))
			newMerged, results := dmp.PatchApply(patches, merged)
			for _, applied := range results {
				if !applied {
					conflicts = append(conflicts, file)
					break
				}
			}
			if len(conflicts) > 0 {
				break
			}
			merged = newMerged
		}
		if len(conflicts) > 0 {
			break
		}
		finalDiff := formatDiff(dmp.DiffMain(baseline, merged, false))
		mergedFiles[file] = FileVersion{
			Timestamp: time.Now(),
			Content:   merged,
			Diff:      finalDiff,
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
	// Only remove the merged commits.
	vm.commits = remainingCommits
	log.Printf("Created version %d with tag '%s' merging commits: %v", mergedVersion.ID, tag, commitIDs)
	return mergedVersion, nil
}

// RevertPendingCommits clears all pending commits.
func (vm *VersionManager) RevertPendingCommits() {
	vm.Lock()
	defer vm.Unlock()
	vm.commits = []Commit{}
	log.Println("Pending commits reverted.")
}

// AbortMerge is provided to simulate an abort (like git merge --abort).
func (vm *VersionManager) AbortMerge() {
	vm.Lock()
	defer vm.Unlock()
	log.Println("Merge aborted. No changes applied.")
}

// GetDiff returns a diff between the stored latest file content and the supplied new content.
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

	// Process file events.
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

// handleChanges returns file diffs (changes) as seen by the file watcher.
func handleChanges(w http.ResponseWriter, r *http.Request) {
	changes := versionManager.GetChanges()
	json.NewEncoder(w).Encode(changes)
}

// Commit creation API.
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

// Merge all pending commits into a version.
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

// Revert pending commits.
func handleRevertCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.RevertPendingCommits()
	w.Write([]byte("Pending commits reverted."))
}

// Abort an ongoing merge.
func handleAbortMerge(w http.ResponseWriter, r *http.Request) {
	versionManager.AbortMerge()
	w.Write([]byte("Merge aborted."))
}

// List all created versions.
func handleGetVersions(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	json.NewEncoder(w).Encode(versionManager.versions)
}

// Switch the deployed version.
type SwitchVersionPayload struct {
	VersionID int `json:"version_id"`
}

func handleSwitchVersion(w http.ResponseWriter, r *http.Request) {
	var payload SwitchVersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	versionManager.Lock()
	defer versionManager.Unlock()
	var selected *VersionGroup
	for _, ver := range versionManager.versions {
		if ver.ID == payload.VersionID {
			selected = &ver
			break
		}
	}
	if selected == nil {
		http.Error(w, "Version not found", http.StatusNotFound)
		return
	}
	versionManager.deployedVersion = selected
	log.Printf("Switched to deployed version %d", selected.ID)
	json.NewEncoder(w).Encode(selected)
}

// Retrieve the deployed version.
func handleDeployedVersion(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	if versionManager.deployedVersion == nil {
		http.Error(w, "No deployed version", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(versionManager.deployedVersion)
}

// Serve the frontend HTML page.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

// --- Main ---

func main() {
	go watchFiles(watchPaths)

	// API endpoints.
	http.HandleFunc("/api/changes", handleChanges)
	http.HandleFunc("/api/commit", handleCommit)
	http.HandleFunc("/api/commits", handleGetCommits)
	http.HandleFunc("/api/version", handleCreateVersion)
	http.HandleFunc("/api/version/mergeSelected", handleMergeSelectedCommits)
	http.HandleFunc("/api/version/revert", handleRevertCommits)
	http.HandleFunc("/api/merge/abort", handleAbortMerge)
	http.HandleFunc("/api/versions", handleGetVersions)
	http.HandleFunc("/api/version/switch", handleSwitchVersion)
	http.HandleFunc("/api/deployedVersion", handleDeployedVersion)
	http.HandleFunc("/", handleIndex)

	// Serve static files from ./static.
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
