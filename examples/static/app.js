document.addEventListener("DOMContentLoaded", () => {
    // Cache DOM elements
    const devModeBtn = document.getElementById("devModeBtn");
    const prodModeBtn = document.getElementById("prodModeBtn");
    const branchSwitchBtn = document.getElementById("branchSwitchBtn");
    const branchInput = document.getElementById("branchInput");
    const devSection = document.getElementById("devSection");
    const prodSection = document.getElementById("prodSection");

    const fileChangesDiv = document.getElementById("fileChanges");
    const fileSelectList = document.getElementById("fileSelectList");
    const commitMsgInput = document.getElementById("commitMsgInput");
    const commitBtn = document.getElementById("commitBtn");
    const commitsListDiv = document.getElementById("commitsList");
    const mergeTagInput = document.getElementById("mergeTagInput");
    const mergeSelectedBtn = document.getElementById("mergeSelectedBtn");
    const selectAllCommits = document.getElementById("selectAllCommits");

    const versionsListDiv = document.getElementById("versionsList");
    const switchVersionBtn = document.getElementById("switchVersionBtn");
    const rollbackVersionBtn = document.getElementById("rollbackVersionBtn");
    const deployedVersionDiv = document.getElementById("deployedVersion");

    // Helper: get authentication headers.
    const getAuthHeaders = () => {
        const credentials = btoa("admin:supersecret"); // In production, load securely.
        return {
            "Content-Type": "application/json",
            "Authorization": "Basic " + credentials
        };
    };

    // Mode switching functions
    const showDevMode = () => {
        devSection.classList.remove("hidden");
        prodSection.classList.add("hidden");
        loadFileChanges();
        loadCommits();
    };

    const showProdMode = () => {
        prodSection.classList.remove("hidden");
        devSection.classList.add("hidden");
        loadVersions();
        loadDeployedVersion();
    };

    // Attach Mode Switching Event Listeners
    devModeBtn.addEventListener("click", showDevMode);
    prodModeBtn.addEventListener("click", showProdMode);

    // Switch Branch.
    branchSwitchBtn.addEventListener("click", () => {
        const branch = branchInput.value.trim();
        if (!branch) {
            alert("Please enter a branch name.");
            return;
        }
        const payload = { branch };
        fetch("/api/branch/switch", {
            method: "POST",
            headers: getAuthHeaders(),
            body: JSON.stringify(payload)
        })
            .then(response => {
                if (!response.ok) {
                    return response.text().then(err => { throw new Error(err); });
                }
                alert("Switched branch to " + branch);
                // Reload commits and versions for the current branch.
                loadCommits();
                loadVersions();
            })
            .catch(err => alert("Error switching branch: " + err.message));
    });

    // Global Select All for Pending Commits.
    selectAllCommits.addEventListener("change", () => {
        const isChecked = selectAllCommits.checked;
        const checkboxes = document.getElementsByClassName("mergeCommitCheckbox");
        Array.from(checkboxes).forEach(cb => (cb.checked = isChecked));
    });

    // Load file changes (with diff hidden in consistent accordion)
    const loadFileChanges = async () => {
        try {
            const res = await fetch("/api/changes", { headers: getAuthHeaders() });
            const data = await res.json();
            let changesHTML = "";
            let fileListHTML = "";
            for (const file in data) {
                if (!data[file].trim()) continue;
                const diffLines = data[file]
                    .split("\n")
                    .map(line => {
                        if (line.startsWith("+")) return `<span class="text-green-600">${line}</span>`;
                        else if (line.startsWith("-")) return `<span class="text-red-600">${line}</span>`;
                        return `<span>${line}</span>`;
                    })
                    .join("<br>");
                changesHTML += `
          <div class="mb-4 border-b pb-2">
            <h3 class="font-semibold">${file}</h3>
            <details class="mt-2">
              <summary>View Changes</summary>
              <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${diffLines}</pre>
            </details>
          </div>
        `;
                fileListHTML += `
          <div>
            <label class="flex items-center">
              <input type="checkbox" class="commitFile mr-2" value="${file}"> ${file}
            </label>
          </div>
        `;
            }
            fileChangesDiv.innerHTML = changesHTML || `<p class="text-gray-500">No changes detected.</p>`;
            fileSelectList.innerHTML = fileListHTML || `<p class="text-gray-500">No files available for commit.</p>`;
        } catch (error) {
            fileChangesDiv.innerHTML = `<p class="text-red-500">Error loading changes.</p>`;
            console.error(error);
        }
    };

    // Load Pending Commits
    const loadCommits = async () => {
        try {
            const res = await fetch("/api/commits", { headers: getAuthHeaders() });
            const commits = await res.json() || [];
            let html = "";
            commits.forEach(commit => {
                let filesHTML = "";
                for (const file in commit.files) {
                    filesHTML += `
            <details class="ml-4 mb-2 border rounded">
              <summary>File: ${file}</summary>
              <div class="p-2">
                <p class="font-medium">Content:</p>
                <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${commit.files[file].content}</pre>
                <p class="font-medium">Diff:</p>
                <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${commit.files[file].diff || "No diff"}</pre>
              </div>
            </details>
          `;
                }
                html += `
          <details class="mb-4 border rounded">
            <summary class="flex items-center">
              <input type="checkbox" class="mergeCommitCheckbox mr-2" value="${commit.id}">
              <span>Commit ${commit.id} – ${commit.message}</span>
              <span class="text-sm text-gray-600 ml-2">[${new Date(commit.timestamp).toLocaleString()}]</span>
            </summary>
            <div class="p-2">${filesHTML}</div>
          </details>
        `;
            });
            commitsListDiv.innerHTML = html || `<p class="text-gray-500">No pending commits.</p>`;
        } catch (error) {
            commitsListDiv.innerHTML = `<p class="text-red-500">Error loading commits.</p>`;
            console.error(error);
        }
    };

    // Load Versions (Production Mode)
    const loadVersions = async () => {
        try {
            const [versionsRes, deployedRes] = await Promise.all([
                fetch("/api/versions", { headers: getAuthHeaders() }),
                fetch("/api/deployedVersion", { headers: getAuthHeaders() })
            ]);
            const versions = await versionsRes.json();
            const deployedVersion = deployedRes.ok ? await deployedRes.json() : null;
            const deployedID = deployedVersion ? deployedVersion.id : null;
            let html = "";
            versions.forEach(ver => {
                let filesHTML = "";
                for (const file in ver.files) {
                    filesHTML += `
            <details class="ml-4 mb-2 border rounded">
              <summary>File: ${file}</summary>
              <div class="p-2">
                <p class="font-medium">Content:</p>
                <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${ver.files[file].content}</pre>
              </div>
            </details>
          `;
                }
                const checked = ver.id === deployedID ? "checked" : "";
                html += `
          <div class="mb-4 border rounded p-2">
            <label class="flex items-center">
              <input type="radio" name="versionRadio" value="${ver.id}" ${checked} class="mr-2">
              <span>Version ${ver.id} ${ver.tag ? "- " + ver.tag : ""}</span>
              <span class="text-sm text-gray-600 ml-2">[${new Date(ver.timestamp).toLocaleString()}]</span>
            </label>
            <details class="mt-2 border rounded">
              <summary>Show Version Details</summary>
              <div class="p-2">
                <p class="font-medium">Commit Message:</p>
                <p>${ver.commitMessage}</p>
                ${filesHTML}
              </div>
            </details>
          </div>
        `;
            });
            versionsListDiv.innerHTML = html || `<p class="text-gray-500">No versions created.</p>`;
            if (!deployedID) {
                deployedVersionDiv.innerHTML = `<p class="text-gray-500">Please select a version to deploy.</p>`;
            }
        } catch (error) {
            versionsListDiv.innerHTML = `<p class="text-red-500">Error loading versions.</p>`;
            console.error(error);
        }
    };

    // Load Deployed Version
    const loadDeployedVersion = async () => {
        try {
            const res = await fetch("/api/deployedVersion", { headers: getAuthHeaders() });
            if (!res.ok) {
                deployedVersionDiv.innerHTML = `<p class="text-gray-500">No version deployed.</p>`;
                return;
            }
            const ver = await res.json();
            let filesHTML = "";
            for (const file in ver.files) {
                filesHTML += `
          <div class="mb-2 border rounded p-2">
            <strong>File: ${file}</strong>
            <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${ver.files[file].content}</pre>
          </div>
        `;
            }
            deployedVersionDiv.innerHTML = `
        <div>
          <h3 class="text-xl font-bold">Deployed Version ${ver.id} ${ver.tag ? "- " + ver.tag : ""}</h3>
          <p class="font-medium">Commit Message:</p>
          <p>${ver.commitMessage}</p>
          ${filesHTML}
        </div>
      `;
        } catch (error) {
            deployedVersionDiv.innerHTML = `<p class="text-red-500">Error loading deployed version.</p>`;
            console.error(error);
        }
    };

    // Create Commit
    commitBtn.addEventListener("click", async () => {
        const commitMsg = commitMsgInput.value.trim();
        const checkboxes = document.getElementsByClassName("commitFile");
        const selectedFiles = [];
        for (const cb of checkboxes) {
            if (cb.checked) selectedFiles.push(cb.value);
        }
        if (!commitMsg || selectedFiles.length === 0) {
            alert("Please enter a commit message and select at least one file.");
            return;
        }
        try {
            await fetch("/api/commit", {
                method: "POST",
                headers: getAuthHeaders(),
                body: JSON.stringify({ message: commitMsg, files: selectedFiles })
            });
            commitMsgInput.value = "";
            loadCommits();
            loadFileChanges();
        } catch (error) {
            console.error(error);
        }
    });

    // Merge Selected Commits
    mergeSelectedBtn.addEventListener("click", async () => {
        const tag = mergeTagInput.value.trim();
        const checkboxes = document.getElementsByClassName("mergeCommitCheckbox");
        const selectedIDs = [];
        for (const cb of checkboxes) {
            if (cb.checked) selectedIDs.push(parseInt(cb.value));
        }
        if (!tag || selectedIDs.length === 0) {
            alert("Please enter a merge tag and select at least one commit.");
            return;
        }
        try {
            const res = await fetch("/api/version/mergeSelected", {
                method: "POST",
                headers: getAuthHeaders(),
                body: JSON.stringify({ tag, commit_ids: selectedIDs })
            });
            if (!res.ok) {
                const err = await res.text();
                throw new Error(err);
            }
            mergeTagInput.value = "";
            loadVersions();
            loadCommits();
            loadFileChanges();
        } catch (error) {
            alert("Merge error: " + error.message);
        }
    });

    // Switch Version – deploys the selected version without altering baseline.
    switchVersionBtn.addEventListener("click", async () => {
        const radios = document.getElementsByName("versionRadio");
        let selected = null;
        for (const r of radios) {
            if (r.checked) {
                selected = r.value;
                break;
            }
        }
        if (!selected) {
            alert("Please select a version.");
            return;
        }
        try {
            const res = await fetch("/api/version/switch", {
                method: "POST",
                headers: getAuthHeaders(),
                body: JSON.stringify({ version_id: parseInt(selected) })
            });
            if (!res.ok) {
                const err = await res.text();
                throw new Error(err);
            }
            loadDeployedVersion();
        } catch (error) {
            alert("Switch version error: " + error.message);
        }
    });

    // Rollback Deployment – deploys an older version and resets baseline.
    rollbackVersionBtn.addEventListener("click", async () => {
        const radios = document.getElementsByName("versionRadio");
        let selected = null;
        for (const r of radios) {
            if (r.checked) {
                selected = r.value;
                break;
            }
        }
        if (!selected) {
            alert("Please select a version to rollback to.");
            return;
        }
        try {
            const res = await fetch("/api/deployment/rollback", {
                method: "POST",
                headers: getAuthHeaders(),
                body: JSON.stringify({ version_id: parseInt(selected) })
            });
            if (!res.ok) {
                const err = await res.text();
                throw new Error(err);
            }
            loadDeployedVersion();
        } catch (error) {
            alert("Rollback error: " + error.message);
        }
    });

    // Initialize in Developer Mode
    showDevMode();
});
