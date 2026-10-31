// Package accountimport 解析账号批量导入输入。
//
// 支持两类来源：本服务标准的 {"accounts":[...]} 文件，以及浏览器导出的
// 单账号 JSON（cookies_raw / localStorage / jwt）。ZIP 通过魔数识别，不信任
// 文件扩展名；解析过程不访问网络、不写磁盘，也不把凭证拼进错误信息。
package accountimport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
)

const (
	// MaxRequestBytes 是应用层允许读取的导入请求总量。
	MaxRequestBytes int64 = 32 << 20
	// MaxZIPEntries 是单个 ZIP 内允许的条目数（含目录）。
	MaxZIPEntries = 200
	// MaxZIPEntryBytes 是单个 ZIP 条目解压后的上限。
	MaxZIPEntryBytes int64 = 8 << 20
	// MaxErrorItems 是响应中最多返回的错误条数。
	MaxErrorItems = 100
)

// Input 是一个上传文件。
type Input struct {
	Name string
	Data []byte
}

// Item 是一条已规范化、可直接反序列化为池账号的 JSON。
// ID 单独返回，便于导入层在不接触凭证的前提下分类重复账号。
type Item struct {
	ID   string
	JSON json.RawMessage
}

// Result 是一次批量解析的统计与结果。
type Result struct {
	Accounts        []Item
	Total           int
	Valid           int
	Invalid         int
	Errors          []string
	ErrorsTruncated bool
}

type parser struct {
	result        Result
	expandedBytes int64
}

// Parse 解析一组上传文件。单个文件或 ZIP 内的坏条目不会中断其余条目。
func Parse(inputs []Input) Result {
	p := &parser{result: Result{Accounts: []Item{}, Errors: []string{}}}
	if len(inputs) == 0 {
		p.invalid("request", "没有上传文件")
		return p.result
	}

	var totalBytes int64
	for _, input := range inputs {
		if int64(len(input.Data)) > MaxRequestBytes-totalBytes {
			p.invalid(safeSource(input.Name), fmt.Sprintf("请求总量超过 %dMiB 上限", MaxRequestBytes>>20))
			return p.result
		}
		totalBytes += int64(len(input.Data))
	}
	for _, input := range inputs {
		p.parseInput(input)
	}
	return p.result
}

func (p *parser) parseInput(input Input) {
	source := safeSource(input.Name)
	if len(input.Data) == 0 {
		p.invalid(source, "文件为空")
		return
	}
	if isZIP(input.Data) {
		p.parseZIP(source, input.Data)
		return
	}
	p.parseJSON(source, input.Data)
}

func (p *parser) parseZIP(source string, data []byte) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		p.invalid(source, "ZIP 文件损坏或无法读取")
		return
	}
	if len(reader.File) > MaxZIPEntries {
		p.invalid(source, fmt.Sprintf("ZIP 条目超过 %d 个上限", MaxZIPEntries))
		return
	}

	for _, entry := range reader.File {
		entryName := safeSource(entry.Name)
		if entry.FileInfo().IsDir() || shouldSkipZIPEntry(entry.Name) {
			continue
		}
		if entry.UncompressedSize64 > uint64(MaxZIPEntryBytes) {
			p.invalid(entryName, fmt.Sprintf("ZIP 条目超过 %dMiB 上限", MaxZIPEntryBytes>>20))
			continue
		}
		if p.expandedBytes+int64(entry.UncompressedSize64) > MaxRequestBytes {
			p.invalid(source, fmt.Sprintf("ZIP 解压总量超过 %dMiB 上限", MaxRequestBytes>>20))
			return
		}
		file, err := entry.Open()
		if err != nil {
			p.invalid(entryName, "ZIP 条目无法打开")
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(file, MaxZIPEntryBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			p.invalid(entryName, "ZIP 条目读取失败")
			continue
		}
		if int64(len(content)) > MaxZIPEntryBytes {
			p.invalid(entryName, fmt.Sprintf("ZIP 条目超过 %dMiB 上限", MaxZIPEntryBytes>>20))
			continue
		}
		p.expandedBytes += int64(len(content))
		p.parseJSON(entryName, content)
	}
}

func (p *parser) parseJSON(source string, data []byte) {
	if int64(len(data)) > MaxRequestBytes {
		p.invalid(source, fmt.Sprintf("文件超过 %dMiB 上限", MaxRequestBytes>>20))
		return
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		p.invalid(source, "不是合法 JSON 对象")
		return
	}
	if raw, ok := root["accounts"]; ok {
		p.parseStandard(source, raw)
		return
	}
	if looksLikeBrowserExport(root) {
		item, err := parseBrowserExport(root)
		if err != nil {
			p.invalid(source, err.Error())
			return
		}
		p.valid(item)
		return
	}
	p.invalid(source, "不支持的 JSON 结构（需要 accounts 数组或浏览器导出字段）")
}

func (p *parser) parseStandard(source string, raw json.RawMessage) {
	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		p.invalid(source, "accounts 必须是数组")
		return
	}
	if len(records) == 0 {
		p.invalid(source, "accounts 数组为空")
		return
	}
	for index, record := range records {
		item, err := normalizeStandard(record)
		if err != nil {
			p.invalid(fmt.Sprintf("%s 第 %d 项", source, index+1), err.Error())
			continue
		}
		p.valid(item)
	}
}

func (p *parser) valid(item Item) {
	p.result.Accounts = append(p.result.Accounts, item)
	p.result.Valid++
	p.result.Total++
}

func (p *parser) invalid(source, reason string) {
	p.result.Invalid++
	p.result.Total++
	if len(p.result.Errors) >= MaxErrorItems {
		p.result.ErrorsTruncated = true
		return
	}
	p.result.Errors = append(p.result.Errors, source+": "+reason)
}

func isZIP(data []byte) bool {
	return len(data) >= 2 && data[0] == 'P' && data[1] == 'K'
}

func shouldSkipZIPEntry(name string) bool {
	clean := strings.ReplaceAll(name, "\\", "/")
	base := path.Base(clean)
	return strings.HasPrefix(clean, "__MACOSX/") || strings.HasPrefix(base, ".")
}

func safeSource(name string) string {
	if strings.TrimSpace(name) == "" {
		return "未命名文件"
	}
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, name)
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120]) + "…"
	}
	return name
}
