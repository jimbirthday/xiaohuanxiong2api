package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"xhx2api/internal/accountimport"
	"xhx2api/internal/pool"
)

var errImportTooLarge = errors.New("导入请求超过 32MiB 上限")

// adminImportAccounts 实现 POST /admin/api/accounts/import。
//
// 接收 multipart 的 files/file，可同时上传多个 JSON 与 ZIP；每条坏记录只影响
// 自己。dry_run 只分类不写盘，overwrite 决定同 ID 是跳过还是替换。
func (s *Server) adminImportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, accountimport.MaxRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Message: fmt.Sprintf("导入请求超过 %dMiB 上限", accountimport.MaxRequestBytes>>20),
				Type:    typeInvalidRequest,
				Code:    "import_too_large",
			})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{
			Message: "解析 multipart 上传失败：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_multipart",
		})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if r.MultipartForm == nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请使用 multipart/form-data 上传文件",
			Type:    typeInvalidRequest,
			Code:    "invalid_multipart",
		})
		return
	}

	headers := append([]*multipart.FileHeader{}, r.MultipartForm.File["files"]...)
	headers = append(headers, r.MultipartForm.File["file"]...)
	if len(headers) == 0 {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请至少上传一个 files/file 文件",
			Type:    typeInvalidRequest,
			Code:    "import_files_required",
		})
		return
	}
	if len(headers) > accountimport.MaxZIPEntries {
		writeError(w, http.StatusBadRequest, apiError{
			Message: fmt.Sprintf("一次最多上传 %d 个文件", accountimport.MaxZIPEntries),
			Type:    typeInvalidRequest,
			Code:    "too_many_import_files",
		})
		return
	}

	inputs, err := readImportInputs(headers)
	if err != nil {
		if errors.Is(err, errImportTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Message: err.Error(),
				Type:    typeInvalidRequest,
				Code:    "import_too_large",
			})
		} else {
			writeError(w, http.StatusBadRequest, apiError{
				Message: err.Error(),
				Type:    typeInvalidRequest,
				Code:    "import_read_failed",
			})
		}
		return
	}
	parsed := accountimport.Parse(inputs)
	accounts := make([]*pool.Account, 0, len(parsed.Accounts))
	for _, item := range parsed.Accounts {
		var account pool.Account
		if err := json.Unmarshal(item.JSON, &account); err != nil {
			writeError(w, http.StatusInternalServerError, apiError{
				Message: "导入记录无法转换为账号",
				Type:    typeUpstream,
				Code:    "import_convert_failed",
			})
			return
		}
		accounts = append(accounts, &account)
	}

	overwrite := importBool(r.FormValue("overwrite"))
	dryRun := importBool(r.FormValue("dry_run"))
	imported, err := s.pool.ImportAccounts(accounts, pool.ImportOptions{
		Overwrite: overwrite,
		DryRun:    dryRun,
	})
	if err != nil {
		s.logger.Warnf("[admin] 批量导入写盘失败：%v", err)
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "批量导入写盘失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "persist_failed",
		})
		return
	}

	allErrors := append([]string{}, parsed.Errors...)
	allErrors = append(allErrors, imported.Errors...)
	truncated := parsed.ErrorsTruncated || imported.ErrorsTruncated || len(allErrors) > accountimport.MaxErrorItems
	if len(allErrors) > accountimport.MaxErrorItems {
		allErrors = allErrors[:accountimport.MaxErrorItems]
	}
	if allErrors == nil {
		allErrors = []string{}
	}
	invalid := parsed.Invalid + imported.Invalid
	total := parsed.Total
	if total < imported.Total+invalid {
		total = imported.Total + invalid
	}

	s.logger.Infof("[admin] 批量导入完成 dry_run=%v overwrite=%v total=%d added=%d updated=%d skipped=%d invalid=%d",
		dryRun, overwrite, total, imported.Added, imported.Updated, imported.Skipped, invalid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"dry_run":          dryRun,
		"overwrite":        overwrite,
		"total":            total,
		"added":            imported.Added,
		"updated":          imported.Updated,
		"skipped":          imported.Skipped,
		"invalid":          invalid,
		"errors":           allErrors,
		"errors_truncated": truncated,
	})
}

func readImportInputs(headers []*multipart.FileHeader) ([]accountimport.Input, error) {
	inputs := make([]accountimport.Input, 0, len(headers))
	var total int64
	for _, header := range headers {
		file, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("读取上传文件失败")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, accountimport.MaxRequestBytes-total+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("读取上传文件失败")
		}
		total += int64(len(data))
		if total > accountimport.MaxRequestBytes {
			return nil, errImportTooLarge
		}
		inputs = append(inputs, accountimport.Input{Name: header.Filename, Data: data})
	}
	return inputs, nil
}

func importBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
