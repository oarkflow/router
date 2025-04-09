package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/fsnotify/fsnotify"
	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	Username = "admin"       // In production load securely (e.g. via env vars)
	Password = "supersecret" // In production load securely (e.g. via env vars)
	dbFile   = "versionmanager.db"
)

var watchPaths = []string{"./configs"}

// --- Entity Definitions ---

// FileVersion holds file contents and diff info.
type FileVersion struct {
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
	Diff      string    `json:"diff,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
}

// Commit represents a commit with a set of file versions.
type Commit struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Message   string                 `json:"message"`
	Branch    string                 `json:"branch"`
	Files     map[string]FileVersion `json:"files"`
}

// VersionGroup represents a merged version (tag) that can be deployed.
type VersionGroup struct {
	ID            int                    `json:"id"`
	Tag           string                 `json:"tag,omitempty"`
	CommitMessage string                 `json:"commitMessage,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Branch        string                 `json:"branch"`
	Files         map[string]FileVersion `json:"files"`
}

// ManagerState is a snapshot of all persistent state.
type ManagerState struct {
	LatestFiles    map[string]string        `json:"latestFiles"`
	FileVersions   map[string][]FileVersion `json:"fileVersions"`
	PendingCommits []Commit                 `json:"pendingCommits"`
	CommittedFiles map[string]string        `json:"committedFiles"`
	Versions       []VersionGroup           `json:"versions"`
	NextCommitID   int                      `json:"nextCommitId"`
	NextVerID      int                      `json:"nextVerId"`
	CurrentBranch  string                   `json:"currentBranch"`
	AuditLog       []string                 `json:"auditLog"`
}

// --- Persistent Storage with bbolt ---

const stateBucket = "State"
const stateKey = "manager"

// Storage encapsulates our bbolt DB.
type Storage struct {
	db *bbolt.DB
}

// NewStorage opens (or creates) a Bolt database and ensures the state bucket exists.
func NewStorage(dbFile string) (*Storage, error) {
	db, err := bbolt.Open(dbFile, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, err
	}
	// Create state bucket if not exists.
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(stateBucket))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Storage{db: db}, nil
}

// SaveState persists a ManagerState in the state bucket.
func (s *Storage) SaveState(state ManagerState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(stateBucket))
		return b.Put([]byte(stateKey), data)
	})
}

// LoadState attempts to load a ManagerState from the database.
func (s *Storage) LoadState() (ManagerState, error) {
	var state ManagerState
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(stateBucket))
		data := b.Get([]byte(stateKey))
		if data == nil {
			return errors.New("state not found")
		}
		return json.Unmarshal(data, &state)
	})
	return state, err
}

// --- Version Manager ---

// VersionManager holds versioning data and a pointer to the storage engine.
type VersionManager struct {
	sync.RWMutex
	LatestFiles     map[string]string        // latest file content snapshot
	FileVersions    map[string][]FileVersion // history of file versions per file
	PendingCommits  []Commit                 // pending commits for current branch
	CommittedFiles  map[string]string        // baseline committed file contents per branch
	Versions        []VersionGroup           // merged version groups
	NextCommitID    int                      // auto-increment commit ID
	NextVerID       int                      // auto-increment version group ID
	CurrentBranch   string                   // current branch name
	AuditLog        []string                 // audit log entries
	DeployedVersion *VersionGroup            // deployed version
	storage         *Storage                 // persistent storage engine
}

// persistState writes the entire manager state to storage.
func (vm *VersionManager) persistState() error {
	state := ManagerState{
		LatestFiles:    vm.LatestFiles,
		FileVersions:   vm.FileVersions,
		PendingCommits: vm.PendingCommits,
		CommittedFiles: vm.CommittedFiles,
		Versions:       vm.Versions,
		NextCommitID:   vm.NextCommitID,
		NextVerID:      vm.NextVerID,
		CurrentBranch:  vm.CurrentBranch,
		AuditLog:       vm.AuditLog,
	}
	return vm.storage.SaveState(state)
}

// NewVersionManager initializes a new manager; it attempts to load existing state.
func NewVersionManager(storage *Storage) *VersionManager {
	vm := &VersionManager{
		LatestFiles:     make(map[string]string),
		FileVersions:    make(map[string][]FileVersion),
		PendingCommits:  []Commit{},
		CommittedFiles:  make(map[string]string),
		Versions:        []VersionGroup{},
		NextCommitID:    1,
		NextVerID:       1,
		CurrentBranch:   "main",
		AuditLog:        []string{},
		storage:         storage,
		DeployedVersion: nil,
	}
	// Attempt to load previous state.
	if state, err := storage.LoadState(); err == nil {
		vm.LatestFiles = state.LatestFiles
		vm.FileVersions = state.FileVersions
		vm.PendingCommits = state.PendingCommits
		vm.CommittedFiles = state.CommittedFiles
		vm.Versions = state.Versions
		vm.NextCommitID = state.NextCommitID
		vm.NextVerID = state.NextVerID
		vm.CurrentBranch = state.CurrentBranch
		vm.AuditLog = state.AuditLog
		log.Println("Loaded persisted state.")
	} else {
		log.Println("No previous state found; starting new.")
		vm.persistState()
	}
	return vm
}

var versionManager *VersionManager

// --- Diff and Merge Functions ---

// formatDiff produces a unified diff string using diffmatchpatch.
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

// mergeFileVersions applies each candidate’s change (using patching) sequentially to the base.
// Returns the final merged content and a conflict flag.
func mergeFileVersions(base string, candidates []string) (string, bool) {
	merged := base
	dmp := diffmatchpatch.New()
	conflictOccurred := false
	for _, candidate := range candidates {
		patches := dmp.PatchMake(merged, candidate)
		result, applied := dmp.PatchApply(patches, merged)
		for _, ok := range applied {
			if !ok {
				conflictOccurred = true
				break
			}
		}
		if conflictOccurred {
			break
		}
		merged = result
	}
	return merged, conflictOccurred
}

// --- Deployment Function ---

// deployVersion stages files to a temporary folder and atomically swaps it with production.
func deployVersion(ver VersionGroup) error {
	tempDir := "Prod_temp"
	if err := os.RemoveAll(tempDir); err != nil {
		return fmt.Errorf("failed to clear temp folder: %v", err)
	}
	for srcPath, fileVersion := range ver.Files {
		relPath := strings.TrimPrefix(srcPath, "configs/")
		destPath := filepath.Join(tempDir, relPath)
		destDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", destDir, err)
		}
		if fileVersion.Deleted {
			continue
		}
		if err := os.WriteFile(destPath, []byte(fileVersion.Content), 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %v", destPath, err)
		}
		log.Printf("Staged %s", destPath)
	}
	prodDir := "Prod"
	backupDir := "Prod_backup"
	if _, err := os.Stat(prodDir); err == nil {
		os.RemoveAll(backupDir)
		if err := os.Rename(prodDir, backupDir); err != nil {
			return fmt.Errorf("failed to backup production folder: %v", err)
		}
	}
	if err := os.Rename(tempDir, prodDir); err != nil {
		os.Rename(backupDir, prodDir)
		return fmt.Errorf("failed to deploy new version: %v", err)
	}
	os.RemoveAll(backupDir)
	log.Printf("Deployed new version to production.")
	return nil
}

// --- Version Manager Methods ---
// Each method that changes state calls persistState() to write the latest state.

func (vm *VersionManager) UpdateFile(path, content string, deleted bool) {
	vm.Lock()
	defer vm.Unlock()
	version := FileVersion{Timestamp: time.Now(), Content: content, Deleted: deleted}
	vm.LatestFiles[path] = content
	vm.FileVersions[path] = append(vm.FileVersions[path], version)
	entry := fmt.Sprintf("%s updated at %s (deleted=%v)", path, time.Now().Format(time.RFC3339), deleted)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("File updated: %s (deleted=%v)", path, deleted)
}

func (vm *VersionManager) GetChanges() map[string]string {
	vm.RLock()
	defer vm.RUnlock()
	changes := make(map[string]string)
	dmp := diffmatchpatch.New()
	for file, versions := range vm.FileVersions {
		baseline := ""
		if c, ok := vm.CommittedFiles[file]; ok {
			baseline = strings.TrimSpace(c)
		}
		latest := strings.TrimSpace(versions[len(versions)-1].Content)
		if versions[len(versions)-1].Deleted {
			latest = ""
		}
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

func (vm *VersionManager) CreateCommit(selectedFiles []string, message string) Commit {
	vm.Lock()
	defer vm.Unlock()
	dmp := diffmatchpatch.New()
	commit := Commit{
		ID:        vm.NextCommitID,
		Timestamp: time.Now(),
		Message:   message,
		Branch:    vm.CurrentBranch,
		Files:     make(map[string]FileVersion),
	}
	for _, file := range selectedFiles {
		if versions, exists := vm.FileVersions[file]; exists && len(versions) > 0 {
			baseline := ""
			if c, ok := vm.CommittedFiles[file]; ok {
				baseline = strings.TrimSpace(c)
			}
			currentVersion := versions[len(versions)-1]
			current := strings.TrimSpace(currentVersion.Content)
			diffs := dmp.DiffMain(baseline, current, false)
			dmp.DiffCleanupSemantic(diffs)
			diffText := formatDiff(diffs)
			fv := FileVersion{
				Timestamp: time.Now(),
				Content:   current,
				Diff:      diffText,
				Deleted:   currentVersion.Deleted,
			}
			commit.Files[file] = fv
			vm.CommittedFiles[file] = current
			vm.FileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: current, Deleted: currentVersion.Deleted}}
		}
	}
	vm.PendingCommits = append(vm.PendingCommits, commit)
	vm.NextCommitID++
	// Record audit entry.
	entry := fmt.Sprintf("Commit %d created on branch '%s'", commit.ID, vm.CurrentBranch)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("Created commit %d on branch '%s': %s", commit.ID, vm.CurrentBranch, message)
	return commit
}

func (vm *VersionManager) MergeCommits(tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]string)
	var conflicts []string
	for _, commit := range vm.PendingCommits {
		if commit.Branch != vm.CurrentBranch {
			continue
		}
		mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
		for file, version := range commit.Files {
			candidate := strings.TrimSpace(version.Content)
			fileCommits[file] = append(fileCommits[file], candidate)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	dmp := diffmatchpatch.New()
	for file, candidates := range fileCommits {
		base := ""
		if b, ok := vm.CommittedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged, conflict := mergeFileVersions(base, candidates)
		if conflict {
			conflicts = append(conflicts, file)
			continue
		}
		finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
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
		ID:            vm.NextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Branch:        vm.CurrentBranch,
		Files:         mergedFiles,
	}
	vm.Versions = append(vm.Versions, mergedVersion)
	vm.NextVerID++
	// Remove merged commits (only for current branch).
	var remaining []Commit
	for _, commit := range vm.PendingCommits {
		if commit.Branch != vm.CurrentBranch {
			remaining = append(remaining, commit)
		}
	}
	vm.PendingCommits = remaining
	entry := fmt.Sprintf("Merged commits on branch '%s' into version %d", vm.CurrentBranch, mergedVersion.ID)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("Created version %d on branch '%s' with tag '%s'", mergedVersion.ID, vm.CurrentBranch, tag)
	return mergedVersion, nil
}

func (vm *VersionManager) MergeSelectedCommits(commitIDs []int, tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]string)
	var conflicts []string
	selectedMap := make(map[int]bool)
	for _, id := range commitIDs {
		selectedMap[id] = true
	}
	var remaining []Commit
	for _, commit := range vm.PendingCommits {
		if commit.Branch != vm.CurrentBranch {
			remaining = append(remaining, commit)
			continue
		}
		if selectedMap[commit.ID] {
			mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
			for file, fv := range commit.Files {
				fileCommits[file] = append(fileCommits[file], strings.TrimSpace(fv.Content))
			}
		} else {
			remaining = append(remaining, commit)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	dmp := diffmatchpatch.New()
	for file, candidates := range fileCommits {
		base := ""
		if b, ok := vm.CommittedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged, conflict := mergeFileVersions(base, candidates)
		if conflict {
			conflicts = append(conflicts, file)
			continue
		}
		finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
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
		ID:            vm.NextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Branch:        vm.CurrentBranch,
		Files:         mergedFiles,
	}
	vm.Versions = append(vm.Versions, mergedVersion)
	vm.NextVerID++
	vm.PendingCommits = remaining
	entry := fmt.Sprintf("Merged selected commits on branch '%s' into version %d", vm.CurrentBranch, mergedVersion.ID)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("Created version %d on branch '%s' with tag '%s' merging commits: %v", mergedVersion.ID, vm.CurrentBranch, tag, commitIDs)
	return mergedVersion, nil
}

func (vm *VersionManager) RevertPendingCommits() {
	vm.Lock()
	defer vm.Unlock()
	var remaining []Commit
	for _, commit := range vm.PendingCommits {
		if commit.Branch != vm.CurrentBranch {
			remaining = append(remaining, commit)
		}
	}
	vm.PendingCommits = remaining
	entry := fmt.Sprintf("Pending commits on branch '%s' reverted.", vm.CurrentBranch)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Println("Pending commits reverted.")
}

func (vm *VersionManager) AbortMerge() {
	vm.Lock()
	defer vm.Unlock()
	entry := fmt.Sprintf("Merge aborted on branch '%s'", vm.CurrentBranch)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Println("Merge aborted. No changes applied.")
}

func (vm *VersionManager) GetDiff(filePath, newContent string) string {
	vm.RLock()
	defer vm.RUnlock()
	baseline := ""
	if content, ok := vm.LatestFiles[filePath]; ok {
		baseline = content
	}
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseline, newContent, false)
	dmp.DiffCleanupSemantic(diffs)
	return formatDiff(diffs)
}

func (vm *VersionManager) SwitchBranch(branch string) {
	vm.Lock()
	defer vm.Unlock()
	vm.CurrentBranch = branch
	entry := fmt.Sprintf("Switched to branch '%s'", branch)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("Switched to branch: %s", branch)
}

func (vm *VersionManager) RollbackDeployment(versionID int) error {
	vm.Lock()
	defer vm.Unlock()
	var target *VersionGroup
	for _, ver := range vm.Versions {
		if ver.ID == versionID && ver.Branch == vm.CurrentBranch {
			target = &ver
			break
		}
	}
	if target == nil {
		return errors.New("version not found for rollback on current branch")
	}
	if err := deployVersion(*target); err != nil {
		return err
	}
	for file, fv := range target.Files {
		vm.CommittedFiles[file] = fv.Content
		vm.FileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: fv.Content, Deleted: fv.Deleted}}
	}
	vm.PendingCommits = []Commit{}
	vm.DeployedVersion = target
	entry := fmt.Sprintf("Rolled back deployment to version %d on branch '%s'", target.ID, vm.CurrentBranch)
	vm.AuditLog = append(vm.AuditLog, entry)
	_ = vm.persistState()
	log.Printf("Rolled back deployment to version %d", target.ID)
	return nil
}

// --- File Watcher ---

func watchFiles(paths []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	for _, root := range paths {
		err = filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.HasSuffix(info.Name(), "~") {
				return nil
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
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if strings.HasSuffix(event.Name, "~") {
				continue
			}
			if event.Op&fsnotify.Remove != 0 || event.Op&fsnotify.Rename != 0 {
				versionManager.UpdateFile(event.Name, "", true)
				log.Printf("File removed: %s", event.Name)
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				data, err := ioutil.ReadFile(event.Name)
				if err != nil {
					log.Printf("Error reading %s: %v", event.Name, err)
					continue
				}
				content := string(data)
				diff := versionManager.GetDiff(event.Name, content)
				log.Printf("Change on %s\nDiff:\n%s", event.Name, diff)
				versionManager.UpdateFile(event.Name, content, false)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}

// --- HTTP Handlers and Basic Auth ---

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != Username || p != Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized.", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func handleChanges(w http.ResponseWriter, r *http.Request) {
	changes := versionManager.GetChanges()
	_ = json.NewEncoder(w).Encode(changes)
}

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
	_ = json.NewEncoder(w).Encode(commit)
}

func handleGetCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.RLock()
	defer versionManager.RUnlock()
	_ = json.NewEncoder(w).Encode(versionManager.PendingCommits)
}

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
	_ = json.NewEncoder(w).Encode(ver)
}

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
	_ = json.NewEncoder(w).Encode(ver)
}

func handleRevertCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.RevertPendingCommits()
	_, _ = w.Write([]byte("Pending commits reverted."))
}

func handleAbortMerge(w http.ResponseWriter, r *http.Request) {
	versionManager.AbortMerge()
	_, _ = w.Write([]byte("Merge aborted."))
}

func handleGetVersions(w http.ResponseWriter, r *http.Request) {
	versionManager.RLock()
	defer versionManager.RUnlock()
	_ = json.NewEncoder(w).Encode(versionManager.Versions)
}

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
	for _, ver := range versionManager.Versions {
		if ver.ID == payload.VersionID && ver.Branch == versionManager.CurrentBranch {
			selected = &ver
			break
		}
	}
	if selected == nil {
		http.Error(w, "Version not found", http.StatusNotFound)
		return
	}
	if err := deployVersion(*selected); err != nil {
		http.Error(w, fmt.Sprintf("Failed to deploy version: %v", err), http.StatusInternalServerError)
		return
	}
	// For switching, we do not alter the committed baseline.
	versionManager.DeployedVersion = selected
	entry := fmt.Sprintf("Switched to deployed version %d on branch '%s'", selected.ID, versionManager.CurrentBranch)
	versionManager.AuditLog = append(versionManager.AuditLog, entry)
	_ = versionManager.persistState()
	log.Printf("Switched to deployed version %d", selected.ID)
	_ = json.NewEncoder(w).Encode(selected)
}

func handleDeployedVersion(w http.ResponseWriter, r *http.Request) {
	versionManager.RLock()
	defer versionManager.RUnlock()
	if versionManager.DeployedVersion == nil {
		http.Error(w, "No deployed version", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(versionManager.DeployedVersion)
}

type SwitchBranchPayload struct {
	Branch string `json:"branch"`
}

func handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	var payload SwitchBranchPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.Branch) == "" {
		http.Error(w, "Invalid branch payload", http.StatusBadRequest)
		return
	}
	versionManager.SwitchBranch(payload.Branch)
	_, _ = w.Write([]byte(fmt.Sprintf("Switched to branch '%s'", payload.Branch)))
}

type RollbackPayload struct {
	VersionID int `json:"version_id"`
}

func handleRollback(w http.ResponseWriter, r *http.Request) {
	var payload RollbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if err := versionManager.RollbackDeployment(payload.VersionID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_, _ = w.Write([]byte(fmt.Sprintf("Rolled back deployment to version %d", payload.VersionID)))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

func main() {
	// Initialize storage.
	storage, err := NewStorage(dbFile)
	if err != nil {
		log.Fatalf("Error opening storage: %v", err)
	}
	defer storage.db.Close()
	// Initialize version manager.
	versionManager = NewVersionManager(storage)
	// Start file watcher.
	go watchFiles(watchPaths)
	// Set up HTTP routes with basic auth for sensitive endpoints.
	http.HandleFunc("/api/changes", basicAuth(handleChanges))
	http.HandleFunc("/api/commit", basicAuth(handleCommit))
	http.HandleFunc("/api/commits", basicAuth(handleGetCommits))
	http.HandleFunc("/api/version", basicAuth(handleCreateVersion))
	http.HandleFunc("/api/version/mergeSelected", basicAuth(handleMergeSelectedCommits))
	http.HandleFunc("/api/version/revert", basicAuth(handleRevertCommits))
	http.HandleFunc("/api/merge/abort", basicAuth(handleAbortMerge))
	http.HandleFunc("/api/versions", basicAuth(handleGetVersions))
	http.HandleFunc("/api/version/switch", basicAuth(handleSwitchVersion))
	http.HandleFunc("/api/deployedVersion", basicAuth(handleDeployedVersion))
	http.HandleFunc("/api/branch/switch", basicAuth(handleSwitchBranch))
	http.HandleFunc("/api/deployment/rollback", basicAuth(handleRollback))
	http.HandleFunc("/", handleIndex)
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
