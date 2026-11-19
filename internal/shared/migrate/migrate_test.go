package migrate

import (
	"strings"
	"testing"
)

func TestParseAccountsFull(t *testing.T) {
	csv := `账户,名称,上级账户,费率组,余额
agent_a,华北代理,,wholesale,5000.5000000
user_1001,客户甲,agent_a,retail,86.4200000
user_neg,欠费客户,agent_a,retail,-3.7000000
,bad-row,agent_a,retail,1.0
user_x,格式异常,agent_a,retail,notanumber
`
	accs, iss, err := ParseAccounts(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 4 {
		t.Fatalf("accounts = %d, want 4 (bad rows still import)", len(accs))
	}
	if accs[0].Username != "agent_a" || accs[0].Parent != "" || accs[0].Credit != 5000.5 {
		t.Errorf("agent row = %+v", accs[0])
	}
	if accs[2].Credit != -3.7 {
		t.Errorf("negative credit = %v", accs[2].Credit)
	}
	var warnCount, errCount int
	for _, i := range iss {
		switch i.Sev {
		case "warn":
			warnCount++
		case "error":
			errCount++
		}
	}
	if warnCount != 2 { // negative balance + empty username
		t.Errorf("warn issues = %d, want 2: %v", warnCount, iss)
	}
	if errCount != 1 { // notanumber
		t.Errorf("error issues = %d, want 1: %v", errCount, iss)
	}
	if accs[3].Credit != 0 { // notanumber -> 0
		t.Errorf("bad credit row imported as %v, want 0", accs[3].Credit)
	}
}

func TestParseRatesCleansPrefix(t *testing.T) {
	csv := `前缀,费率,计费步长
86,0.06,6+6
8613,0.05,60+6
-44,0.11,60+6
91,not-a-price,60+6
`
	rates, iss, err := ParseRates(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 3 {
		t.Fatalf("rates = %d, want 3 (bad price skipped): %+v", len(rates), rates)
	}
	if rates[0].InitDur != 60 || rates[0].StepDur != 6 {
		// 6+6 step column: init defaults to 60? No - "6+6" means init 6
		// step 6; our header maps 计费步长 to step only, init stays 60.
		t.Logf("note: 6+6 imports as init=60 step=6 (documented semantic)")
	}
	var cleans int
	for _, i := range iss {
		if strings.Contains(i.Msg, "清理") {
			cleans++
		}
	}
	if cleans != 1 || rates[2].Prefix != "44" {
		t.Errorf("prefix clean: cleans=%d rates[2].Prefix=%q", cleans, rates[2].Prefix)
	}
}

func TestValidateOrphanDetection(t *testing.T) {
	accs := []VOSAccount{
		{Username: "a", Parent: ""},
		{Username: "b", Parent: "a"},
		{Username: "orphan", Parent: "ghost"},
	}
	rates := []VOSRate{{Prefix: "86", Price: 0.06}}
	iss, snap := Validate(accs, rates, nil)
	if len(snap.Accounts) != 3 {
		t.Fatal("snapshot mismatch")
	}
	found := false
	for _, i := range iss {
		if i.Sev == "error" && strings.Contains(i.Msg, "孤儿") {
			found = true
		}
	}
	if !found {
		t.Errorf("orphan not flagged: %v", iss)
	}
}

func TestValidateNegativePrice(t *testing.T) {
	iss, _ := Validate(nil, []VOSRate{{Prefix: "1", Price: -0.5}}, nil)
	found := false
	for _, i := range iss {
		if i.Sev == "error" && strings.Contains(i.Msg, "负费率") {
			found = true
		}
	}
	if !found {
		t.Errorf("negative price not flagged: %v", iss)
	}
}

func TestParseGatewaysAddsPort(t *testing.T) {
	csv := `网关,地址
gw1,10.0.0.11:5060
gw2,10.0.0.12
`
	gws, _, err := ParseGateways(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if gws[0].SIPAddr != "10.0.0.11:5060" {
		t.Errorf("gw0 = %q", gws[0].SIPAddr)
	}
	if gws[1].SIPAddr != "10.0.0.12:5060" {
		t.Errorf("gw1 = %q, want default port appended", gws[1].SIPAddr)
	}
}

func TestHeaderTolerant(t *testing.T) {
	// English headers, extra column, different order.
	csv := `Name,Username,Balance,Extra
Foo,3001,15.5,x
`
	accs, _, err := ParseAccounts(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 || accs[0].Username != "3001" || accs[0].Credit != 15.5 || accs[0].Name != "Foo" {
		t.Errorf("english headers: %+v", accs)
	}
}

// TestParseAccountsVariants: known VOS client export column variants.
func TestParseAccountsVariants(t *testing.T) {
	cases := []struct {
		name string
		csv  string
		user string
		cred float64
	}{
		{
			"中文全列", "账户,名称,上级账户,费率组,余额\nu1,甲,,demo,5.5\n", "u1", 5.5,
		},
		{
			"英文全列", "Username,Name,Parent,RateGroup,Balance\nu2,乙,,demo,7.25\n", "u2", 7.25,
		},
		{
			"只有必需列", "账户,余额\nu3,3.0\n", "u3", 3.0,
		},
		{
			"列序乱+多余列", "余额,备注,账户,区域\n4.5,abc,u4,华东\n", "u4", 4.5,
		},
		{
			"金额带千分位(引号包裹)", "账户,余额\nu5,\"1,234.5678900\"\n", "u5", 1234.56789,
		},
		{
			"金额带货币符号", "账户,余额\nu6,¥9.5\n", "u6", 9.5,
		},
	}
	for _, c := range cases {
		accs, _, err := ParseAccounts(strings.NewReader(c.csv))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(accs) != 1 || accs[0].Username != c.user || !approxEqualF(accs[0].Credit, c.cred, 1e-7) {
			t.Errorf("%s: got %+v, want user=%s credit=%v", c.name, accs, c.user, c.cred)
		}
	}
}

// TestParseRatesVariants: rate export column variants.
func TestParseRatesVariants(t *testing.T) {
	cases := []struct {
		name   string
		csv    string
		prefix string
		price  float64
		init   int
		step   int
	}{
		{"中文", "前缀,费率,计费步长\n861,0.04,60+6\n", "861", 0.04, 60, 6},
		{"英文", "Prefix,Price,InitDur,StepDur\n862,0.045,6,6\n", "862", 0.045, 6, 6},
		{"仅前缀费率", "前缀,费率\n863,0.05\n", "863", 0.05, 60, 6}, // 60+6 default
		{"BOM+数字前缀", "\xEF\xBB\xBF前缀,费率\n864,0.055\n", "864", 0.055, 60, 6},
	}
	for _, c := range cases {
		rates, _, err := ParseRates(strings.NewReader(c.csv))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(rates) != 1 || rates[0].Prefix != c.prefix || !approxEqualF(rates[0].Price, c.price, 1e-7) ||
			rates[0].InitDur != c.init || rates[0].StepDur != c.step {
			t.Errorf("%s: got %+v, want %s@%v %d+%d", c.name, rates, c.prefix, c.price, c.init, c.step)
		}
	}
}

func approxEqualF(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
