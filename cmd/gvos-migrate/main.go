// Command gvos-migrate imports VOS exports into a G-VOS deployment.
//
// Pipeline:
//   gvos-migrate extract -accounts a.csv -rates r.csv -gateways g.csv -out snapshot.json
//   gvos-migrate validate -snapshot snapshot.json
//   gvos-migrate plan     -snapshot snapshot.json -out plan.md
//   gvos-migrate load     -snapshot snapshot.json -api http://host:8080 -token T [-dry-run]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lixuanqun/gvos-core/internal/shared/migrate"
)

func main() {
	cmd := "usage"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "extract":
		err = cmdExtract(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "load":
		err = cmdLoad(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  gvos-migrate extract  -accounts FILE -rates FILE -gateways FILE -out snapshot.json
  gvos-migrate validate -snapshot snapshot.json
  gvos-migrate plan     -snapshot snapshot.json -out plan.md
  gvos-migrate load     -snapshot snapshot.json -api URL -token T [-dry-run]
`)
}

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	aPath := fs.String("accounts", "", "VOS account CSV")
	rPath := fs.String("rates", "", "VOS rate CSV")
	gPath := fs.String("gateways", "", "VOS gateway CSV")
	out := fs.String("out", "snapshot.json", "output snapshot file")
	fs.Parse(args)

	var accs []migrate.VOSAccount
	var rates []migrate.VOSRate
	var gws []migrate.VOSGateway
	var allIss []migrate.Issue

	if *aPath != "" {
		f, err := os.Open(*aPath)
		if err != nil {
			return err
		}
		defer f.Close()
		a, iss, err := migrate.ParseAccounts(f)
		if err != nil {
			return fmt.Errorf("accounts: %w", err)
		}
		accs, allIss = a, append(allIss, iss...)
	}
	if *rPath != "" {
		f, err := os.Open(*rPath)
		if err != nil {
			return err
		}
		defer f.Close()
		r, iss, err := migrate.ParseRates(f)
		if err != nil {
			return fmt.Errorf("rates: %w", err)
		}
		rates, allIss = r, append(allIss, iss...)
	}
	if *gPath != "" {
		f, err := os.Open(*gPath)
		if err != nil {
			return err
		}
		defer f.Close()
		g, iss, err := migrate.ParseGateways(f)
		if err != nil {
			return fmt.Errorf("gateways: %w", err)
		}
		gws, allIss = g, append(allIss, iss...)
	}

	snap := migrate.Snapshot{Accounts: accs, Rates: rates, Gateways: gws}
	blob, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		return err
	}
	fmt.Printf("extracted: %d accounts, %d rates, %d gateways -> %s\n",
		len(accs), len(rates), len(gws), *out)
	for _, i := range allIss {
		fmt.Printf("  [%s] line %d: %s\n", i.Sev, i.Line, i.Msg)
	}
	return nil
}

func loadSnapshot(path string) (migrate.Snapshot, error) {
	var s migrate.Snapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	snapPath := fs.String("snapshot", "snapshot.json", "snapshot file")
	fs.Parse(args)

	s, err := loadSnapshot(*snapPath)
	if err != nil {
		return err
	}
	iss, _ := migrate.Validate(s.Accounts, s.Rates, s.Gateways)
	errs := 0
	for _, i := range iss {
		fmt.Printf("[%s] %s\n", i.Sev, i.Msg)
		if i.Sev == "error" {
			errs++
		}
	}
	fmt.Printf("validate: %d accounts, %d rates, %d gateways; %d error(s)\n",
		len(s.Accounts), len(s.Rates), len(s.Gateways), errs)
	if errs > 0 {
		return fmt.Errorf("校验发现 %d 个错误，修复后再 load", errs)
	}
	return nil
}

func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	snapPath := fs.String("snapshot", "snapshot.json", "snapshot file")
	out := fs.String("out", "plan.md", "plan markdown output")
	fs.Parse(args)

	s, err := loadSnapshot(*snapPath)
	if err != nil {
		return err
	}
	iss, _ := migrate.Validate(s.Accounts, s.Rates, s.Gateways)

	var b strings.Builder
	fmt.Fprintf(&b, "# VOS -> G-VOS 迁移计划\n\n生成：%s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "## 概览\n\n- 账户 %d 个（含代理层级）\n- 费率 %d 条\n- 网关 %d 个\n\n", len(s.Accounts), len(s.Rates), len(s.Gateways))
	fmt.Fprintf(&b, "## 语义差异决策（默认保持 VOS 行为）\n\n")
	fmt.Fprintf(&b, "| # | VOS 行为 | G-VOS 行为 | 决策 |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| 1 | 计费步长 60+6 直迁 | 同 | 保持 VOS |\n| 2 | 7 位小数第 8 位进一 | 同 | 保持 VOS |\n| 3 | 负余额账户照常呼 | 预付余额 >= 0 | 快照结转为 0 |\n| 4 | 上级费率组缺省 | 需指定 | 落 default 组 |\n\n")
	fmt.Fprintf(&b, "## 校验发现\n\n")
	for _, i := range iss {
		fmt.Fprintf(&b, "- [%s] %s\n", i.Sev, i.Msg)
	}
	fmt.Fprintf(&b, "\n## 切换步骤\n\n1. load --dry-run 导入影子租户核对\n2. 并行双跑 7 天（compare 每日对账）\n3. cutover：网关分 10%% 灰度 1 天后全量\n\n")
	return os.WriteFile(*out, []byte(b.String()), 0o644)
}

func cmdLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	snapPath := fs.String("snapshot", "snapshot.json", "snapshot file")
	api := fs.String("api", "http://127.0.0.1:8080", "G-VOS API base URL")
	token := fs.String("token", "", "admin bearer token")
	dry := fs.Bool("dry-run", false, "validate only, do not write")
	fs.Parse(args)

	s, err := loadSnapshot(*snapPath)
	if err != nil {
		return err
	}
	iss, _ := migrate.Validate(s.Accounts, s.Rates, s.Gateways)
	for _, i := range iss {
		if i.Sev == "error" {
			return fmt.Errorf("校验未通过：%s（先跑 validate）", i.Msg)
		}
	}
	if *dry {
		fmt.Printf("dry-run: would load %d accounts, %d rates, %d gateways into %s\n",
			len(s.Accounts), len(s.Rates), len(s.Gateways), *api)
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	var created, rated int

	// Accounts: create then top up (credit can be negative in VOS -
	// created at 0 then adjusted).
	for _, a := range s.Accounts {
		body := map[string]any{
			"username": a.Username, "name": pick(a.Name, a.Username),
			"rate_group": pick(a.RateGroup, "demo"),
		}
		id, err := postJSON(client, *api+"/v1/admin/accounts", *token, body)
		if err != nil {
			return fmt.Errorf("create account %s: %w", a.Username, err)
		}
		created++
		if a.Credit > 0 {
			if _, err := postJSON(client, *api+"/v1/admin/topup", *token,
				map[string]any{"account_id": id, "delta": a.Credit, "reason": "vos-migration"}); err != nil {
				return fmt.Errorf("topup account %s: %w", a.Username, err)
			}
		}
		// credit <= 0: negative VOS balances import as 0 (the debt
		// stays in the VOS books for the final settlement).
	}

	// Rates: upsert each prefix.
	for _, r := range s.Rates {
		body := map[string]any{
			"prefix": r.Prefix, "price": r.Price,
			"init_dur": r.InitDur, "step_dur": r.StepDur,
		}
		if _, err := postJSON(client, *api+"/v1/admin/rates", *token, body); err != nil {
			return fmt.Errorf("upsert rate %s: %w", r.Prefix, err)
		}
		rated++
	}

	fmt.Printf("load complete: %d accounts (+credits), %d rates imported to %s\n",
		created, rated, *api)
	fmt.Printf("gateways: %d 记录在案（网关注册走控制台/API，下一步 cutover 前配置）\n", len(s.Gateways))
	return nil
}

func postJSON(c *http.Client, url, token string, body any) (string, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		msg := ""
		if out != nil {
			if e, ok := out["error"].(string); ok {
				msg = e
			}
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if id, ok := out["id"].(string); ok {
		return id, nil
	}
	return "", nil
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
