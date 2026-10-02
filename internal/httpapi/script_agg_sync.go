package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Aggregated mode merges the openids of all accounts into a single
// &-delimited env value on QingLong for every script that has a known
// env-name mapping. This matches the multi-account pattern already used
// by zsunny6658_ql_main wxapp scripts (e.g. zhanma.js reads
// process.env.zmnlxq which holds "openid1&openid2&...").
//
// Env-name resolution priority (in scriptEnvMapping table):
//  1. source=manual: user set it explicitly via the API
//  2. source=ground: auto-seeded from an existing QL env value
//  3. source=auto:   inferred by grep'ing the script source
//
// Cron definitions and schedules are NOT touched. Only the env value
// is refreshed when an account is added/removed/rescanned.
const (
	scriptEnvSourceAuto   = "auto"
	scriptEnvSourceManual = "manual"
	scriptEnvSourceGround = "ground"

	scriptEnvJoiner = "&"
)

// AggregateSyncResult describes one aggregated-sync run.
type AggregateSyncResult struct {
	ScannedScripts  int      `json:"scanned_scripts"`
	UpdatedEnvs     []string `json:"updated_envs"`
	FailedScripts   []string `json:"failed_scripts"`
	ScriptEnvHits   []string `json:"script_env_hits"`
	ScriptEnvMisses []string `json:"script_env_misses"`
}

// resolveScriptEnvName returns the QingLong env variable name for the
// given script_key, preferring the mapping table and falling back to
// static source scanning. The returned source flag tells the caller
// whether to persist the inferred value into the mapping table.
func (a *App) resolveScriptEnvName(ctx context.Context, scriptKey string) (envName, source string, err error) {
	// 1) Mapping from DB.
	if m, lookupErr := a.db.GetScriptEnvMapping(ctx, scriptKey); lookupErr == nil {
		return m.EnvName, m.Source, nil
	}

	// 2) Static grep of the mounted script source (recurses subdirs).
	mountRoot := strings.TrimSpace(a.cfg.QingLongScriptsDir)
	if mountRoot == "" {
		return "", "", fmt.Errorf("YYB_QL_SCRIPTS_DIR not configured; cannot scan script source")
	}
	name, scanErr := grepOpenidEnvFromSource(mountRoot, scriptKey)
	if scanErr != nil {
		return "", "", scanErr
	}
	if name == "" {
		return "", "", fmt.Errorf("no candidate env name in script source for %s", scriptKey)
	}
	return name, scriptEnvSourceAuto, nil
}

// syncAggregated refreshes the &-delimited env value for every mapping
// row. It does not touch cron definitions or schedules.
func (a *App) syncAggregated(ctx context.Context) (*AggregateSyncResult, error) {
	if !a.qinglong.configured() {
		return nil, fmt.Errorf("面板 OpenAPI 未配置")
	}

	accounts, err := a.db.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return emptyAggregateResult(), nil
	}

	// Collect all account openids. The current QL setup applies the same
	// openid list to every script env, so we aggregate all known accounts.
	allOpenids := []string{}
	seen := map[string]bool{}
	for _, acc := range accounts {
		oid := strings.TrimSpace(acc.OpenID)
		if oid == "" {
			oid = fmt.Sprintf("%d", acc.ID)
		}
		if !seen[oid] {
			seen[oid] = true
			allOpenids = append(allOpenids, oid)
		}
	}
	if len(allOpenids) == 0 {
		return emptyAggregateResult(), nil
	}
	sort.Strings(allOpenids)
	newValue := strings.Join(allOpenids, scriptEnvJoiner)
	remarks := fmt.Sprintf("YYB 聚合多账号（%d 个）", len(allOpenids))

	// Iterate the mapping table: script_key -> env_name.
	mappings, err := a.db.ListScriptEnvMappings(ctx)
	if err != nil {
		return nil, err
	}

	result := emptyAggregateResult()
	// Deduplicate by env_name: many scripts share the same wx_openid env,
	// we upsert once per distinct env variable.
	envByKey := map[string][]string{} // env_name -> script keys
	for _, m := range mappings {
		envName := strings.TrimSpace(m.EnvName)
		if envName == "" {
			result.ScriptEnvMisses = append(result.ScriptEnvMisses, m.ScriptKey)
			continue
		}
		result.ScriptEnvHits = append(result.ScriptEnvHits, m.ScriptKey)
		envByKey[envName] = append(envByKey[envName], m.ScriptKey)
	}
	for envName := range envByKey {
		if err := a.qinglong.upsertEnv(ctx, envName, newValue, remarks); err != nil {
			result.FailedScripts = append(result.FailedScripts, envByKey[envName]...)
			continue
		}
		result.UpdatedEnvs = append(result.UpdatedEnvs, envName)
	}
	sort.Strings(result.UpdatedEnvs)
	sort.Strings(result.FailedScripts)
	sort.Strings(result.ScriptEnvHits)
	sort.Strings(result.ScriptEnvMisses)
	return result, nil
}

func emptyAggregateResult() *AggregateSyncResult {
	return &AggregateSyncResult{
		UpdatedEnvs: []string{}, FailedScripts: []string{},
		ScriptEnvHits: []string{}, ScriptEnvMisses: []string{},
	}
}

// handleQingLongSyncAggregated is the HTTP entry point for a manual
// "aggregate-sync" trigger. Also invoked from autoSyncAfterScan.
func (a *App) handleQingLongSyncAggregated(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil && !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.qinglong.configured() {
		writeError(w, http.StatusConflict, "请先配置面板 OpenAPI")
		return
	}
	a.panelSyncMu.Lock()
	defer a.panelSyncMu.Unlock()
	result, err := a.syncAggregated(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- Script source scanner ----

// processEnvRe captures every `process.env.NAME` reference in a JS file.
var processEnvRe = regexp.MustCompile(`process\.env\.([A-Za-z_][A-Za-z0-9_]*)`)

// System / infrastructure variables that must be excluded from
// openid-env candidates.
var excludedEnvNames = map[string]bool{
	"ALL_PROXY": true, "HTTPS_PROXY": true, "HTTP_PROXY": true,
	"all_proxy": true, "http_proxy": true, "https_proxy": true,
	"NODE_PATH": true, "PATH": true, "HOME": true, "USER": true,
	"SHELL": true, "LANG": true, "TZ": true, "PWD": true,
	"PUSH_KEY": true, "PUSH_PLUS_TOKEN": true, "PUSH_PLUS_USER": true,
	"QYWX_KEY": true, "YYB_SERVER": true, "WX_AUTH": true, "wx_auth": true,
	"wx_server_url": true, "WX_SERVER_URL": true, "TASK_BEFORE": true,
}

// grepOpenidEnvFromSource walks the mount root and finds a script whose
// filename stem matches the scriptKey basename; if the scriptKey is a
// sub-path (e.g. "wxapp/mxbc.js"), the basename is matched anywhere
// under mountRoot. Returns the top-ranked openid env variable name.
func grepOpenidEnvFromSource(mountRoot, scriptKey string) (string, error) {
	targetBase := filepath.Base(scriptKey)
	var candidates []string
	err := filepath.WalkDir(mountRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) != targetBase {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".js" && ext != ".py" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if name := inferOpenidEnvName(data, path); name != "" {
			candidates = append(candidates, name)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", mountRoot, err)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no openid env found in %s", scriptKey)
	}
	return candidates[0], nil
}

// inferOpenidEnvName ranks process.env.X references by likelihood of
// holding the openid list. Favour:
//  1. Variables used in .split('&') or .split('\n') expressions.
//  2. Variables whose name equals the script file stem.
//  3. Variables referenced near URL params (safe=, openid=, wx_id=).
func inferOpenidEnvName(src []byte, path string) string {
	srcStr := string(src)
	matches := processEnvRe.FindAllStringSubmatch(srcStr, -1)
	seen := map[string]bool{}
	all := []string{}
	for _, m := range matches {
		name := m[1]
		if excludedEnvNames[name] || seen[name] {
			continue
		}
		seen[name] = true
		all = append(all, name)
	}

	// If the script references the shared wx_openid variable, it is almost
	// certainly the openid carrier (these scripts run under the wx-proxy
	// stack: process.env.wx_openid holds the per-account id list).
	if seen["wx_openid"] {
		return "wx_openid"
	}

	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	scored := map[string]int{}
	for _, name := range all {
		s := 0
		if strings.Contains(srcStr, name+".split(\"&\")") ||
			strings.Contains(srcStr, name+".split('\\n')") {
			s += 100
		}
		if strings.ToLower(name) == stem {
			s += 50
		}
		for _, hint := range []string{
			"safe=" + name, "openid=" + name, "wx_id=" + name,
			"`safe=${" + name + "}", "wx_id=${" + name + "}",
		} {
			if strings.Contains(srcStr, hint) {
				s += 10
			}
		}
		scored[name] = s
	}

	type item struct {
		name  string
		score int
	}
	list := make([]item, 0, len(scored))
	for n, sc := range scored {
		list = append(list, item{n, sc})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].name < list[j].name
	})
	for _, it := range list {
		if it.score > 0 {
			return it.name
		}
	}
	return ""
}

// seedGroundTruthFromQL seeds the mapping table from the mounted QingLong
// script sources. For each wxapp script it infers the real openid env
// variable from its source (e.g. fhxmh.js reads wx_openid, not "fhxmh").
// Scripts that share the same env variable (wx_openid) collapse onto one
// row keyed by the env name — mirroring the aggregate-sync semantics.
func (a *App) seedGroundTruthFromQL(ctx context.Context) ([]string, error) {
	mountRoot := strings.TrimSpace(a.cfg.QingLongScriptsDir)
	seeded := []string{}
	if mountRoot == "" {
		return nil, fmt.Errorf("脚本目录未挂载（QingLongScriptsDir 为空）")
	}
	type hit struct {
		scriptKey string
		envName   string
	}
	var hits []hit
	err := filepath.WalkDir(mountRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".js" && filepath.Ext(path) != ".py" {
			return nil
		}
		// Only consider scripts under the wxapp directory to reduce noise.
		if !strings.Contains(path, "wxapp") {
			return nil
		}
		// Skip backup/stale repo copies that mirror the wx app scripts.
		if strings.Contains(path, "/bak-") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		envName := inferOpenidEnvName(data, path)
		if envName == "" {
			return nil
		}
		rel, relErr := filepath.Rel(mountRoot, path)
		if relErr != nil {
			return nil
		}
		hits = append(hits, hit{rel, envName})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// We still want to tolerate scripts whose source can't be read but that
	// exist as a env named like the script. But ground-truth first: for a
	// script we could infer from source, use that. For scripts we could NOT
	// infer, fall back to env-name==script-name via QL envs (below).
	//
	// Upsert each script->env from the source scan. Multiple scripts sharing
	// env -> each gets its own row (dedup happens at sync time).
	checked := map[string]bool{}
	for _, h := range hits {
		checked[h.scriptKey] = true
		if _, upErr := a.db.UpsertScriptEnvMapping(ctx, h.scriptKey, h.envName, scriptEnvSourceAuto); upErr != nil {
			continue
		}
		seeded = append(seeded, h.scriptKey+"→"+h.envName)
	}

	// Fallback for scripts with no matching openid var in source but whose
	// basename matches an existing openid-looking QL env (the 72 created
	// earlier). This covers token-class scripts where openid is implicit.
	envs, envErr := a.qinglong.listEnvs(ctx, "")
	if envErr != nil {
		return seeded, nil
	}
	for _, e := range envs {
		value := strings.TrimSpace(e.Value)
		if value == "" || !looksLikeOpenidValue(value) {
			continue
		}
		scriptKey, err := a.inferScriptKeyForEnv(ctx, e.Name)
		if err != nil || scriptKey == "" || checked[scriptKey] {
			continue
		}
		if _, upErr := a.db.UpsertScriptEnvMapping(ctx, scriptKey, e.Name, scriptEnvSourceGround); upErr != nil {
			continue
		}
		seeded = append(seeded, scriptKey+"→"+e.Name)
	}
	return seeded, nil
}

// inferScriptKeyForEnv guesses the script_key for an env var name.
// Priority:
//  1. Recursively grep mounted script sources for `process.env.<envName>`.
//  2. Scan cron commands for a script whose basename stem equals envName.
//  3. Direct filename-stem match in the mount root.
func (a *App) inferScriptKeyForEnv(ctx context.Context, envName string) (string, error) {
	mountRoot := strings.TrimSpace(a.cfg.QingLongScriptsDir)
	repos, _ := qingLongRepoRoots(a.cfg.QingLongRepo)

	// 1. Recursively grep the mount root for `process.env.<envName>`.
	if mountRoot != "" {
		type hit struct {
			scriptKey string
			score     int
		}
		var hits []hit
		err := filepath.WalkDir(mountRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".js" && ext != ".py" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "process.env."+envName) {
				return nil
			}
			rel, relErr := filepath.Rel(mountRoot, path)
			if relErr != nil {
				return nil
			}
			// Prefer paths under configured repos.
			score := 0
			for _, repo := range repos {
				clean := strings.Trim(strings.TrimSpace(repo), "/")
				if strings.HasPrefix(rel, clean+"/") || strings.Contains(rel, "/"+clean+"/") {
					score += 10
					break
				}
			}
			hits = append(hits, hit{rel, score})
			return nil
		})
		if err == nil && len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool {
				if hits[i].score != hits[j].score {
					return hits[i].score > hits[j].score
				}
				return hits[i].scriptKey < hits[j].scriptKey
			})
			return hits[0].scriptKey, nil
		}
	}

	// 2. Cron command basename match.
	if envName != "" {
		crons, listErr := a.qinglong.listCrons(ctx, "")
		if listErr == nil {
			for _, c := range crons {
				cmd := strings.TrimSpace(c.Command)
				for _, p := range []string{"arcadia run ", "task ", "node ", "python3 ", "python "} {
					if strings.HasPrefix(cmd, p) {
						cmd = strings.TrimSpace(strings.TrimPrefix(cmd, p))
						break
					}
				}
				base := strings.TrimSuffix(filepath.Base(cmd), filepath.Ext(filepath.Base(cmd)))
				if strings.EqualFold(base, envName) {
					_, repo, ok := parseScriptKeyFromCron(c, repos)
					if ok {
						return repo + "/" + filepath.Base(cmd), nil
					}
					return filepath.Base(cmd), nil
				}
			}
		}
	}

	// 3. Direct filename-stem match in the mount root.
	if mountRoot != "" {
		type stemHit struct {
			scriptKey string
			score     int
		}
		var stemHits []stemHit
		err := filepath.WalkDir(mountRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".js" && ext != ".py" {
				return nil
			}
			stem := strings.TrimSuffix(filepath.Base(path), ext)
			if !strings.EqualFold(stem, envName) {
				return nil
			}
			rel, relErr := filepath.Rel(mountRoot, path)
			if relErr != nil {
				return nil
			}
			score := 0
			for _, repo := range repos {
				clean := strings.Trim(strings.TrimSpace(repo), "/")
				if strings.HasPrefix(rel, clean+"/") {
					score += 10
					break
				}
			}
			stemHits = append(stemHits, stemHit{rel, score})
			return nil
		})
		if err == nil && len(stemHits) > 0 {
			sort.Slice(stemHits, func(i, j int) bool {
				if stemHits[i].score != stemHits[j].score {
					return stemHits[i].score > stemHits[j].score
				}
				return stemHits[i].scriptKey < stemHits[j].scriptKey
			})
			return stemHits[0].scriptKey, nil
		}
	}
	return "", fmt.Errorf("cannot infer script key for env %s", envName)
}

func looksLikeOpenidValue(value string) bool {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '&' || r == '\n' || r == '\r' || r == ','
	})
	if len(parts) == 0 {
		return false
	}
	openids := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		first := strings.SplitN(p, "#", 2)[0]
		if strings.HasPrefix(first, "ow") && len(first) >= 20 {
			openids++
		}
	}
	return openids >= 1
}

// ---- HTTP handlers for mapping CRUD ----

type scriptEnvMappingIn struct {
	ScriptKey string `json:"script_key"`
	EnvName   string `json:"env_name"`
	Source    string `json:"source"`
}

func (a *App) handleQingLongScriptEnvMappingList(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil && !requireAdmin(w, r) {
		return
	}
	mappings, err := a.db.ListScriptEnvMappings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mappings)
}

func (a *App) handleQingLongScriptEnvMappingUpsert(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil && !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body scriptEnvMappingIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.ScriptKey) == "" || strings.TrimSpace(body.EnvName) == "" {
		writeError(w, http.StatusBadRequest, "script_key and env_name required")
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = scriptEnvSourceManual
	}
	m, err := a.db.UpsertScriptEnvMapping(r.Context(), body.ScriptKey, body.EnvName, source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *App) handleQingLongScriptEnvMappingDelete(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil && !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body scriptEnvMappingIn
	_ = decodeOptionalJSON(r, &body)
	if strings.TrimSpace(body.ScriptKey) == "" {
		writeError(w, http.StatusBadRequest, "script_key required")
		return
	}
	if err := a.db.DeleteScriptEnvMapping(r.Context(), body.ScriptKey); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleQingLongSeedMapping seeds the mapping table from existing QL envs.
func (a *App) handleQingLongSeedMapping(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil && !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.qinglong.configured() {
		writeError(w, http.StatusConflict, "请先配置面板 OpenAPI")
		return
	}
	seeded, err := a.seedGroundTruthFromQL(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seeded": seeded, "count": len(seeded),
	})
}
