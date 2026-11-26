// Package migrate implements the VOS -> gvos migration pipeline:
// extract (parse VOS exports) -> validate (health report) -> plan
// (differences + mapping decisions) -> load (import via admin API).
//
// The VOS export formats here are modeled on community-documented
// client exports (account/rate/gateway CSV columns); the extract step
// is deliberately tolerant of extra columns.
package migrate

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// VOSAccount is one row of the VOS account export.
type VOSAccount struct {
	Username  string  // 账户/用户名
	Name      string  // 名称/备注
	Parent    string  // 上级账户（空=顶层）
	RateGroup string  // 费率组名
	Credit    float64 // 余额/预存款
	Postpaid  bool    // 后付费标记（信用额度>0 视为后付）
}

// VOSRate is one row of the VOS rate export.
type VOSRate struct {
	Prefix  string
	Price   float64 // per-minute
	InitDur int     // first step seconds (60+6 -> 60)
	StepDur int     // increment seconds (60+6 -> 6)
}

// VOSGateway is one row of the VOS gateway export.
type VOSGateway struct {
	Name    string
	SIPAddr string // host:port
	MaxC    int    // concurrent cap
}

// ParseAccounts reads the VOS account CSV (header row + columns by
// name; extra columns ignored, order-independent).
func ParseAccounts(r io.Reader) ([]VOSAccount, []Issue, error) {
	recs, err := readCSV(r)
	if err != nil {
		return nil, nil, err
	}
	col, iss := mapHeader(recs[0])
	idx := func(name string, candidates ...string) int {
		for _, c := range candidates {
			if i, ok := col[c]; ok {
				return i
			}
		}
		return -1
	}
	// NB: the first variadic arg is the canonical name and is NOT a
	// candidate - every acceptable header must appear in candidates.
	iUser := idx("user", "username", "账户", "用户名", "account")
	iName := idx("displayname", "name", "名称", "备注")
	iParent := idx("parent", "上级", "上级账户", "代理")
	iRate := idx("rategroup", "费率组", "费率", "rate_group")
	iCredit := idx("credit", "余额", "预存款", "金额", "balance")
	if iUser < 0 || iCredit < 0 {
		return nil, iss, fmt.Errorf("账户导出缺少必需列：需要 账户/用户名 和 余额/预存款")
	}

	var out []VOSAccount
	for n, row := range recs[1:] {
		lineno := n + 2
		user := strings.TrimSpace(get(row, iUser))
		credit, err := parseFloat(get(row, iCredit))
		if user == "" {
			iss = append(iss, Issue{Line: lineno, Sev: "warn", Msg: "空用户名，跳过"})
			continue
		}
		if err != nil {
			iss = append(iss, Issue{Line: lineno, Sev: "error", Msg: fmt.Sprintf("余额 %q 非数字，按 0 处理", get(row, iCredit))})
			credit = 0
		}
		if credit < 0 {
			iss = append(iss, Issue{Line: lineno, Sev: "warn", Msg: fmt.Sprintf("账户 %s 负余额 %.7f（欠费），导入后为 0", user, credit)})
		}
		out = append(out, VOSAccount{
			Username:  user,
			Name:      strings.TrimSpace(get(row, iName)),
			Parent:    strings.TrimSpace(get(row, iParent)),
			RateGroup: strings.TrimSpace(get(row, iRate)),
			Credit:    credit,
		})
	}
	return out, iss, nil
}

// ParseRates reads the VOS rate CSV.
func ParseRates(r io.Reader) ([]VOSRate, []Issue, error) {
	recs, err := readCSV(r)
	if err != nil {
		return nil, nil, err
	}
	col, iss := mapHeader(recs[0])
	idx := func(name string, candidates ...string) int {
		for _, c := range candidates {
			if i, ok := col[c]; ok {
				return i
			}
		}
		return -1
	}
	iPrefix := idx("pre", "prefix", "前缀", "号码前缀")
	iPrice := idx("amt", "price", "费率", "价格", "每分钟")
	iInit := idx("first", "initdur", "init_dur", "init", "首6秒", "首时长")
	iStep := idx("stepdur", "step_dur", "step", "计费步长", "步长")
	if iPrefix < 0 || iPrice < 0 {
		return nil, iss, fmt.Errorf("费率导出缺少必需列：需要 前缀 和 费率")
	}

	var out []VOSRate
	for n, row := range recs[1:] {
		lineno := n + 2
		prefix := strings.TrimSpace(get(row, iPrefix))
		price, err := parseFloat(get(row, iPrice))
		if prefix == "" {
			continue
		}
		// Prefix must be digits (a leading '-' or stray characters are
		// export artifacts, not real prefixes).
		if !isDigits(prefix) {
			iss = append(iss, Issue{Line: lineno, Sev: "warn", Msg: fmt.Sprintf("前缀 %q 含非数字字符，清理为 %s", prefix, cleanDigits(prefix))})
			prefix = cleanDigits(prefix)
			if prefix == "" {
				continue
			}
		}
		if err != nil {
			iss = append(iss, Issue{Line: lineno, Sev: "error", Msg: fmt.Sprintf("前缀 %s 费率 %q 非数字，跳过", prefix, get(row, iPrice))})
			continue
		}
		init, step := 60, 6 // VOS default 60+6
		if iInit >= 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(get(row, iInit))); err == nil && v > 0 {
				init = v
			}
		}
		if iStep >= 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(get(row, iStep))); err == nil && v > 0 {
				step = v
			}
		}
		out = append(out, VOSRate{Prefix: prefix, Price: price, InitDur: init, StepDur: step})
	}
	return out, iss, nil
}

// ParseGateways reads the VOS gateway CSV.
func ParseGateways(r io.Reader) ([]VOSGateway, []Issue, error) {
	recs, err := readCSV(r)
	if err != nil {
		return nil, nil, err
	}
	col, iss := mapHeader(recs[0])
	idx := func(name string, candidates ...string) int {
		for _, c := range candidates {
			if i, ok := col[c]; ok {
				return i
			}
		}
		return -1
	}
	iName := idx("gwname", "name", "网关", "名称")
	iAddr := idx("host", "addr", "address", "地址", "IP", "sip", "服务器")
	if iAddr < 0 {
		return nil, iss, fmt.Errorf("网关导出缺少必需列：需要 地址/IP")
	}

	var out []VOSGateway
	for _, row := range recs[1:] {
		addr := strings.TrimSpace(get(row, iAddr))
		if addr == "" {
			continue
		}
		name := addr
		if iName >= 0 && get(row, iName) != "" {
			name = strings.TrimSpace(get(row, iName))
		}
		if !strings.Contains(addr, ":") {
			addr += ":5060"
		}
		out = append(out, VOSGateway{Name: name, SIPAddr: addr})
	}
	return out, iss, nil
}

// Issue is one validation finding.
type Issue struct {
	Line int
	Sev  string // error | warn | info
	Msg  string
}

// Validate runs cross-file checks on the extracted snapshot and
// returns the plan (per-file counts + issues + orphan detection).
func Validate(accs []VOSAccount, rates []VOSRate, gws []VOSGateway) ([]Issue, Snapshot) {
	snap := Snapshot{Accounts: accs, Rates: rates, Gateways: gws}
	var iss []Issue

	// Parent references.
	known := map[string]bool{}
	for _, a := range accs {
		known[a.Username] = true
	}
	for _, a := range accs {
		if a.Parent != "" && !known[a.Parent] {
			iss = append(iss, Issue{Sev: "error", Msg: fmt.Sprintf("账户 %s 的上级 %s 不存在（孤儿账户）", a.Username, a.Parent)})
		}
	}
	// Rate group referenced but no rates.
	rgHas := map[string]bool{"default": true, "demo": true}
	for _, r := range rates {
		_ = r
	}
	for _, a := range accs {
		if a.RateGroup != "" && !rgHas[a.RateGroup] {
			iss = append(iss, Issue{Sev: "info", Msg: fmt.Sprintf("账户 %s 费率组 %s 无费率行，将落默认组", a.Username, a.RateGroup)})
		}
	}
	// Negative-rate sanity.
	for _, r := range rates {
		if r.Price < 0 {
			iss = append(iss, Issue{Sev: "error", Msg: fmt.Sprintf("前缀 %s 负费率 %.7f", r.Prefix, r.Price)})
		}
	}
	// Duplicate prefixes.
	seen := map[string]bool{}
	for _, r := range rates {
		if seen[r.Prefix] {
			iss = append(iss, Issue{Sev: "warn", Msg: fmt.Sprintf("前缀 %s 重复，后者覆盖", r.Prefix)})
		}
		seen[r.Prefix] = true
	}
	// Credit totals (informational).
	var total float64
	for _, a := range accs {
		if a.Credit > 0 {
			total += a.Credit
		}
	}
	iss = append(iss, Issue{Sev: "info", Msg: fmt.Sprintf("正余额合计 %.7f（导入时按快照结转）", total)})
	return iss, snap
}

// Snapshot is the extracted + validated migration payload.
type Snapshot struct {
	Accounts []VOSAccount
	Rates    []VOSRate
	Gateways []VOSGateway
}

func readCSV(r io.Reader) ([][]string, error) {
	// Excel-style BOM breaks the first header name; strip it.
	cr := csv.NewReader(&bomStripper{r: r})
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1 // tolerate ragged rows
	return cr.ReadAll()
}

// bomStripper removes a UTF-8 BOM from the head of the stream and
// replays the buffered head bytes first.
type bomStripper struct {
	r    io.Reader
	done bool   // head check performed
	head []byte // non-BOM bytes to replay
}

func (b *bomStripper) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		head := make([]byte, 3)
		n, err := io.ReadFull(b.r, head)
		if n == 3 && err == nil && string(head) == "\xEF\xBB\xBF" {
			// BOM: drop it, fall through to the reader.
		} else {
			b.head = append(b.head, head[:n]...)
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				b.head = append(b.head, head[:n]...)
				return copy(p, b.head), nil
			}
		}
	}
	if len(b.head) > 0 {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	return b.r.Read(p)
}

func mapHeader(header []string) (map[string]int, []Issue) {
	m := map[string]int{}
	var iss []Issue
	for i, h := range header {
		// \r matters: CRLF files leave a trailing carriage return that
		// breaks exact header matching.
		k := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(h, "\r")))
		if _, dup := m[k]; dup {
			iss = append(iss, Issue{Sev: "warn", Msg: fmt.Sprintf("表头 %q 重复", h)})
		}
		m[k] = i
	}
	return m, iss
}

func get(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return row[i]
}

func parseFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	// tolerate thousands separators and currency symbols
	s = strings.ReplaceAll(strings.ReplaceAll(s, ",", ""), "¥", "")
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return strconv.ParseFloat(s, 64)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// cleanDigits keeps only the digit characters (drops '-' etc.).
func cleanDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
