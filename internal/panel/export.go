package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

type accountExportMetadata struct {
	Schema     string            `json:"schema"`
	Provider   string            `json:"provider"`
	Format     string            `json:"format"`
	ExportedAt time.Time         `json:"exported_at"`
	Runtime    json.RawMessage   `json:"runtime"`
	Source     accountExportFile `json:"source_file"`
}

type accountExportFile struct {
	Name        string    `json:"name"`
	ModifiedAt  time.Time `json:"modified_at"`
	Size        int64     `json:"size"`
	Permissions string    `json:"permissions"`
}

func (p *Panel) accountExport(uid string) (json.RawMessage, pool.Status, error) {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return nil, pool.Status{}, os.ErrNotExist
	}
	credential, err := a.ExportRaw()
	if err != nil {
		return nil, pool.Status{}, err
	}
	status, ok := p.cfg.Pool.Status(uid)
	if !ok {
		return nil, pool.Status{}, os.ErrNotExist
	}
	runtime, err := json.Marshal(status)
	if err != nil {
		return nil, pool.Status{}, err
	}
	info, err := os.Stat(a.FilePath)
	if err != nil {
		return nil, pool.Status{}, err
	}
	var document map[string]any
	if err := json.Unmarshal(credential, &document); err != nil || document == nil {
		return nil, pool.Status{}, fmt.Errorf("credential root must be a JSON object")
	}
	if err := addMagpieAccountFields(document, status); err != nil {
		return nil, pool.Status{}, err
	}
	document["credential"] = credential
	document["export_metadata"] = accountExportMetadata{
		Schema:     "workbuddy-manage-panel/account-export/v2",
		Provider:   exportProvider(status.Realm),
		Format:     "magpie/plugin-auth",
		ExportedAt: time.Now().UTC(),
		Runtime:    runtime,
		Source: accountExportFile{
			Name:        filepath.Base(a.FilePath),
			ModifiedAt:  info.ModTime(),
			Size:        info.Size(),
			Permissions: info.Mode().Perm().String(),
		},
	}
	document["pool_status"] = status
	raw, err := json.MarshalIndent(document, "", "  ")
	return raw, status, err
}

type magpieOAuthAccount struct {
	Type             string `json:"type"`
	Access           string `json:"access"`
	Refresh          string `json:"refresh"`
	Expires          int64  `json:"expires"`
	AccountID        string `json:"accountId"`
	UID              string `json:"uid"`
	Domain           string `json:"domain"`
	EnterpriseID     string `json:"enterpriseId,omitempty"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"`
	TokenType        string `json:"tokenType,omitempty"`
}

type storedExportAuth struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	Domain           string `json:"domain"`
	TokenType        string `json:"tokenType"`
}

func addMagpieAccountFields(document map[string]any, status pool.Status) error {
	var credential storedExportAuth
	var stored struct {
		storedExportAuth
		Auth *storedExportAuth `json:"auth"`
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return fmt.Errorf("decode export credentials: %w", err)
	}
	credential = stored.storedExportAuth
	if stored.Auth != nil {
		credential = *stored.Auth
	}
	if credential.AccessToken == "" || credential.RefreshToken == "" {
		return fmt.Errorf("account credentials are missing access or refresh token")
	}
	name := strings.TrimSpace(status.Nickname)
	if name == "" {
		name = status.UID
	}
	account := magpieOAuthAccount{
		Type:             "oauth",
		Access:           credential.AccessToken,
		Refresh:          credential.RefreshToken,
		Expires:          exportMilliseconds(credential.ExpiresAt),
		AccountID:        name,
		UID:              status.UID,
		Domain:           credential.Domain,
		EnterpriseID:     exportEnterpriseID(document),
		RefreshExpiresAt: exportMilliseconds(credential.RefreshExpiresAt),
		TokenType:        credential.TokenType,
	}
	fields := map[string]any{
		"type": account.Type, "access": account.Access, "refresh": account.Refresh,
		"expires": account.Expires, "accountId": account.AccountID, "uid": account.UID,
		"domain": account.Domain,
	}
	if account.EnterpriseID != "" {
		fields["enterpriseId"] = account.EnterpriseID
	}
	if account.RefreshExpiresAt > 0 {
		fields["refreshExpiresAt"] = account.RefreshExpiresAt
	}
	if account.TokenType != "" {
		fields["tokenType"] = account.TokenType
	}
	for key, value := range fields {
		document[key] = value
	}
	return nil
}

func exportEnterpriseID(document map[string]any) string {
	if account, ok := document["account"].(map[string]any); ok {
		if id, _ := account["enterpriseId"].(string); id != "" {
			return id
		}
	}
	id, _ := document["enterpriseId"].(string)
	return id
}

func exportMilliseconds(value int64) int64 {
	if value > 0 && value < 1_000_000_000_000 {
		return value * 1000
	}
	return value
}

func exportProvider(realm string) string {
	if realm == "global" {
		return "workbuddy-ai"
	}
	return "workbuddy"
}

func (p *Panel) accountExportSingle(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	raw, status, err := p.accountExport(uid)
	if err != nil {
		p.accountExportError(w, uid, err)
		return
	}
	name := exportAccountName(status)
	writeAccountDownload(w, name, "application/json; charset=utf-8", raw)
	log.Printf("panel: export account uid=%s format=json", uid)
}

func (p *Panel) accountExportBatch(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if len(payload.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids 不能为空")
		return
	}
	if len(payload.UIDs) > 1000 {
		writeErr(w, http.StatusBadRequest, "单次最多导出 1000 个账号")
		return
	}
	selected := make([]pool.Status, 0, len(payload.UIDs))
	seen := make(map[string]bool, len(payload.UIDs))
	for _, uid := range payload.UIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		status, ok := p.cfg.Pool.Status(uid)
		if !ok {
			writeErr(w, http.StatusNotFound, "account not found: "+uid)
			return
		}
		selected = append(selected, status)
	}
	var documents []struct {
		uid  string
		name string
		raw  json.RawMessage
	}
	for _, status := range selected {
		raw, _, err := p.accountExport(status.UID)
		if err != nil {
			p.accountExportError(w, status.UID, err)
			return
		}
		documents = append(documents, struct {
			uid  string
			name string
			raw  json.RawMessage
		}{uid: status.UID, name: exportAccountName(status), raw: raw})
	}
	if len(documents) == 1 {
		document := documents[0]
		writeAccountDownload(w, document.name, "application/json; charset=utf-8", document.raw)
		log.Printf("panel: export account uid=%s format=json", document.uid)
		return
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	sort.Slice(documents, func(i, j int) bool { return documents[i].name < documents[j].name })
	for _, document := range documents {
		header := &zip.FileHeader{Name: "accounts/" + document.name, Method: zip.Deflate}
		header.SetMode(0o600)
		header.SetModTime(time.Now())
		entry, err := archive.CreateHeader(header)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "create zip: "+err.Error())
			return
		}
		if _, err := entry.Write(document.raw); err != nil {
			writeErr(w, http.StatusInternalServerError, "write zip: "+err.Error())
			return
		}
	}
	if err := archive.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "finish zip: "+err.Error())
		return
	}
	name := fmt.Sprintf("workbuddy-accounts-%d-%s.zip", len(documents), time.Now().Format("20060102-150405"))
	writeAccountDownload(w, name, "application/zip", buffer.Bytes())
	log.Printf("panel: export accounts count=%d format=zip", len(documents))
}

func exportAccountName(status pool.Status) string {
	name := strings.TrimSpace(status.Nickname)
	if name == "" {
		name = status.UID
	}
	name = strings.Map(func(char rune) rune {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune("_.*-", char) {
			return char
		}
		return '_'
	}, name)
	name = strings.Trim(name, "._-")
	if utf8.RuneCountInString(name) > 80 {
		name = string([]rune(name)[:80])
	}
	if name == "" {
		name = "account"
	}
	uid := status.UID
	if !validUID(uid) {
		uid = "account"
	}
	return fmt.Sprintf("workbuddy-%s-%s.json", name, uid)
}

func (p *Panel) accountExportError(w http.ResponseWriter, uid string, err error) {
	if os.IsNotExist(err) {
		writeErr(w, http.StatusNotFound, "account not found: "+uid)
		return
	}
	log.Printf("panel: export account uid=%s failed: %v", uid, err)
	writeErr(w, http.StatusInternalServerError, "账号凭据读取失败，导出已取消")
}

func writeAccountDownload(w http.ResponseWriter, name, contentType string, raw []byte) {
	w.Header().Set("Content-Type", contentType)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if fallback := asciiExportFilename(name); fallback != name {
		encoded := strings.NewReplacer("+", "%20", "%2F", "%252F", "%3B", "%253B").Replace(url.QueryEscape(name))
		disposition = fmt.Sprintf(`attachment; filename=%q; filename*=utf-8''%s`, fallback, encoded)
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Export-Contains-Credentials", "true")
	_, _ = w.Write(raw)
}

func asciiExportFilename(name string) string {
	var out strings.Builder
	for _, char := range name {
		if char < utf8.RuneSelf && (unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune("_.*-", char)) {
			out.WriteRune(char)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}
