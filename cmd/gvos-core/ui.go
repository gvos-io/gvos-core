// Embedded single-file admin console. Zero build step, zero frontend
// dependencies - fetch() + vanilla JS only, so the binary stays
// self-contained (spike tradeoff: production moves to a proper SPA).
package main

const adminHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>G-VOS 管理台</title>
<style>
  :root { --bg:#0f172a; --card:#1e293b; --line:#334155; --fg:#e2e8f0;
          --dim:#94a3b8; --acc:#38bdf8; --ok:#34d399; --warn:#fbbf24; --err:#f87171; }
  * { box-sizing:border-box; margin:0; padding:0; }
  body { background:var(--bg); color:var(--fg); font:14px/1.6 -apple-system,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif; }
  header { display:flex; align-items:center; gap:16px; padding:14px 24px; border-bottom:1px solid var(--line); }
  header h1 { font-size:18px; color:var(--acc); }
  header .tok { margin-left:auto; display:flex; gap:8px; }
  input,button { background:var(--card); border:1px solid var(--line); color:var(--fg); border-radius:6px; padding:6px 10px; font:inherit; }
  input:focus { outline:1px solid var(--acc); }
  button { cursor:pointer; } button:hover { border-color:var(--acc); }
  nav { display:flex; gap:4px; padding:10px 24px 0; }
  nav button { border:none; background:none; color:var(--dim); padding:8px 14px; border-radius:6px 6px 0 0; }
  nav button.on { color:var(--acc); background:var(--card); }
  main { padding:20px 24px; }
  .stats { display:grid; grid-template-columns:repeat(auto-fit,minmax(140px,1fr)); gap:12px; margin-bottom:20px; }
  .stat { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:12px 16px; }
  .stat .k { color:var(--dim); font-size:12px; } .stat .v { font-size:22px; color:var(--acc); }
  table { width:100%; border-collapse:collapse; background:var(--card); border-radius:8px; overflow:hidden; }
  th,td { padding:8px 12px; text-align:left; border-bottom:1px solid var(--line); font-size:13px; }
  th { color:var(--dim); font-weight:600; font-size:12px; text-transform:uppercase; }
  td.num { font-family:ui-monospace,Consolas,monospace; text-align:right; }
  .rowform { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:14px; margin-bottom:16px; display:flex; flex-wrap:wrap; gap:10px; align-items:flex-end; }
  .rowform label { display:flex; flex-direction:column; gap:4px; font-size:12px; color:var(--dim); }
  .badge { display:inline-block; padding:1px 8px; border-radius:99px; font-size:11px; }
  .badge.ok { background:rgba(52,211,153,.15); color:var(--ok); }
  .badge.warn { background:rgba(251,191,36,.15); color:var(--warn); }
  .badge.err { background:rgba(248,113,113,.15); color:var(--err); }
  .msg { margin:10px 0; padding:8px 12px; border-radius:6px; display:none; }
  .msg.ok { background:rgba(52,211,153,.12); color:var(--ok); display:block; }
  .msg.err { background:rgba(248,113,113,.12); color:var(--err); display:block; }
  #refresh { float:right; }
</style>
</head>
<body>
<header>
  <h1>G-VOS 管理台</h1>
  <span id="mode" style="color:var(--dim)"></span>
  <div class="tok">
    <input id="tok" type="password" placeholder="Admin Token" style="width:180px">
    <button onclick="saveTok()">保存</button>
  </div>
</header>
<nav>
  <button id="tab-accounts" onclick="tab('accounts')">账户</button>
  <button id="tab-rates" onclick="tab('rates')">费率</button>
  <button id="tab-cdrs" onclick="tab('cdrs')">话单 CDR</button>
  <button id="rec-btn" onclick="toggleRecord()" style="margin-left:auto;color:var(--warn)">录音: 关</button>
  <button id="lang-btn" onclick="toggleLang()" style="color:var(--dim)">EN</button>
</nav>
<main>
  <div id="msg" class="msg"></div>
  <div class="stats" id="stats"></div>

  <div id="view-accounts" style="display:none">
    <div class="rowform">
      <label>用户名<input id="a-user" placeholder="3000"></label>
      <label>名称<input id="a-name" placeholder="客户A"></label>
      <label>预存款<input id="a-credit" type="number" step="0.0000001" value="10"></label>
      <button onclick="createAccount()">创建账户</button>
      <label>充值账户ID<input id="t-acc" placeholder="account id"></label>
      <label>金额(±)<input id="t-delta" type="number" step="0.0000001" value="10"></label>
      <button onclick="topup()">充值/调整</button>
    </div>
    <table id="tbl-accounts"><thead><tr>
      <th>ID</th><th>名称</th><th>上级</th><th>费率组</th><th>类型</th><th class="num">余额</th>
    </tr></thead><tbody></tbody></table>
  </div>

  <div id="view-rates" style="display:none">
    <div class="rowform">
      <label>前缀<input id="r-prefix" placeholder="8613"></label>
      <label>每分钟价<input id="r-price" type="number" step="0.0000001" value="0.05"></label>
      <label>首步秒<input id="r-init" type="number" value="60"></label>
      <label>步长秒<input id="r-step" type="number" value="6"></label>
      <button onclick="upsertRate()">保存费率</button>
    </div>
    <table id="tbl-rates"><thead><tr>
      <th>前缀</th><th class="num">每分钟价</th><th class="num">首步秒</th><th class="num">步长秒</th><th class="num">最低秒</th>
    </tr></thead><tbody></tbody></table>
  </div>

  <div id="view-cdrs" style="display:none">
    <button id="refresh" onclick="load()">刷新</button>
    <table id="tbl-cdrs"><thead><tr>
      <th>Call-ID</th><th>账户</th><th>被叫</th><th>费率前缀</th>
      <th class="num">通话秒</th><th class="num">计费秒</th><th class="num">费用</th><th>释放原因</th><th>录音</th>
    </tr></thead><tbody></tbody></table>
  </div>
</main>
<script>
let TOK = localStorage.getItem('gvos_tok') || '';
let TAB = 'accounts';
if (TOK) document.getElementById('tok').value = TOK;

function saveTok() {
  TOK = document.getElementById('tok').value;
  localStorage.setItem('gvos_tok', TOK);
  load();
}
function tab(t) {
  TAB = t;
  for (const x of ['accounts','rates','cdrs']) {
    document.getElementById('tab-'+x).classList.toggle('on', x===t);
    document.getElementById('view-'+x).style.display = x===t ? '' : 'none';
  }
  load();
}
function msg(ok, text) {
  const m = document.getElementById('msg');
  m.className = 'msg ' + (ok ? 'ok' : 'err');
  m.textContent = text;
  setTimeout(() => m.className = 'msg', 4000);
}
async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({'Content-Type':'application/json','Authorization':'Bearer '+TOK}, opts.headers||{});
  const r = await fetch(path, opts);
  if (r.status === 401) { msg(false, '鉴权失败：请输入正确的 Admin Token'); throw new Error('401'); }
  const j = await r.json().catch(()=>({}));
  if (!r.ok) { msg(false, (j.error||('HTTP '+r.status))); throw new Error(j.error); }
  return j;
}
function esc(s) { return String(s??'').replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
function fmt(n) { return (n??0).toFixed(7); }

async function load() {
  try {
    const st = await api('/v1/stats');
    document.getElementById('mode').textContent = '模式：' + (st.mode==='postgres'?'PostgreSQL':'内存') + ' · 运行 ' + st.uptime_s + 's';
    document.getElementById('stats').innerHTML =
      stat('在线呼叫', st.live_calls) + stat('累计 CDR', st.cdrs) +
      stat('演示账户余额', (st.caller_bal??0).toFixed(7));
    if (TAB==='accounts') await loadAccounts();
    if (TAB==='rates') await loadRates();
    if (TAB==='cdrs') await loadCDRs();
  } catch (e) { /* msg already shown */ }
}
function stat(k,v) { return '<div class="stat"><div class="k">'+k+'</div><div class="v">'+v+'</div></div>'; }

async function loadAccounts() {
  const j = await api('/v1/admin/accounts');
  document.querySelector('#tbl-accounts tbody').innerHTML = (j.accounts||[]).map(a =>
    '<tr><td>'+esc(a.ID)+'</td><td>'+esc(a.Name||a.ID)+'</td><td>'+esc(a.ParentID||'—')+'</td><td>'+esc(a.RateGroup)+'</td>'+
    '<td>'+(a.Postpaid?'<span class="badge warn">后付</span>':'<span class="badge ok">预付</span>')+'</td>'+
    '<td class="num">'+fmt(a.Credit)+'</td></tr>').join('');
}
async function createAccount() {
  const body = {username:document.getElementById('a-user').value, name:document.getElementById('a-name').value,
                credit:parseFloat(document.getElementById('a-credit').value||'0')};
  const j = await api('/v1/admin/accounts', {method:'POST', body:JSON.stringify(body)});
  msg(true, '已创建 ' + j.username + ' → ' + j.id); loadAccounts();
}
async function topup() {
  const body = {account_id:document.getElementById('t-acc').value, delta:parseFloat(document.getElementById('t-delta').value||'0')};
  const j = await api('/v1/admin/topup', {method:'POST', body:JSON.stringify(body)});
  msg(true, '操作完成，余额 ' + j.balance); loadAccounts();
}
async function loadRates() {
  const j = await api('/v1/admin/rates');
  document.querySelector('#tbl-rates tbody').innerHTML = (j.rates||[]).map(r =>
    '<tr><td>'+esc(r.prefix)+'</td><td class="num">'+fmt(r.price)+'</td><td class="num">'+(r.init_dur||60)+'</td>'+
    '<td class="num">'+(r.step_dur||6)+'</td><td class="num">'+(r.min_dur||0)+'</td></tr>').join('');
}
async function upsertRate() {
  const body = {prefix:document.getElementById('r-prefix').value, price:parseFloat(document.getElementById('r-price').value||'0'),
                init_dur:parseInt(document.getElementById('r-init').value||'60'), step_dur:parseInt(document.getElementById('r-step').value||'6')};
  const j = await api('/v1/admin/rates', {method:'POST', body:JSON.stringify(body)});
  msg(true, '费率已保存：' + j.prefix + ' @ ' + j.price + '/min，下一通电话生效'); loadRates();
}
async function loadCDRs() {
  const j = await api('/v1/admin/cdrs?limit=100');
  document.querySelector('#tbl-cdrs tbody').innerHTML = (j.cdrs||[]).map(c =>
    '<tr><td>'+esc(c.CallID)+'</td><td>'+esc(c.AccountID)+'</td><td>'+esc(c.Dest)+'</td><td>'+esc(c.RatePrefix)+'</td>'+
    '<td class="num">'+(c.Duration??0)+'</td><td class="num">'+(c.BilledSecs??0)+'</td><td class="num">'+fmt(c.Cost)+'</td>'+
    '<td>'+(c.Release==='normal'?'<span class="badge ok">normal</span>':'<span class="badge err">'+esc(c.Release)+'</span>')+'</td>'+
    '<td>'+(c.Recorded?'<span class="badge ok">●</span>':'—')+'</td></tr>').join('');
}
async function toggleRecord() {
  const b = document.getElementById('rec-btn');
  const on = b.textContent.includes('开');
  const j = await api('/v1/admin/record', {method:'POST', body:JSON.stringify({enabled:!on})});
  b.textContent = '录音: ' + (j.enabled ? '开' : '关');
  msg(true, j.enabled ? '录音已开启：下一通电话起生效' : '录音已关闭');
}
// i18n: zh labels with EN fallbacks; toggle re-renders static texts.
const I18N = {
  '账户': 'Accounts', '费率': 'Rates', '话单 CDR': 'CDRs',
  '管理台': 'Console', '模式：': 'mode: ', '内存': 'memory', '在线呼叫': 'Live calls',
  '累计 CDR': 'CDRs', '演示账户余额': 'Demo balance', '用户名': 'Username', '名称': 'Name',
  '预存款': 'Credit', '创建账户': 'Create account', '充值账户ID': 'Top-up account ID',
  '金额(±)': 'Amount(±)', '充值/调整': 'Top up / adjust', '前缀': 'Prefix', '每分钟价': 'Per-min price',
  '首步秒': 'First step(s)', '步长秒': 'Step(s)', '保存费率': 'Save rate',
  '通话秒': 'Talk(s)', '计费秒': 'Billed(s)', '费用': 'Cost', '释放原因': 'Release',
  '录音': 'Recording', '刷新': 'Refresh', '保存': 'Save', '运行': 'uptime',
};
let LANG = localStorage.getItem('gvos_lang') || 'zh';
function applyLang() {
  document.querySelectorAll('button,th,label,.stat .k,header h1').forEach(el => {
    const t = el.textContent.trim();
    if (LANG === 'en' && I18N[t]) el.textContent = I18N[t];
  });
  const rb = document.getElementById('rec-btn');
  if (rb) rb.textContent = (LANG === 'en' ? 'Recording: ' : '录音: ') + (rb.textContent.includes('开') || rb.textContent.includes('on') ? (LANG==='en'?'on':'开') : (LANG==='en'?'off':'关'));
  document.getElementById('lang-btn').textContent = LANG === 'en' ? '中' : 'EN';
}
function toggleLang() {
  LANG = LANG === 'zh' ? 'en' : 'zh';
  localStorage.setItem('gvos_lang', LANG);
  location.reload();
}
tab('accounts');
load();
setTimeout(applyLang, 300);
setInterval(load, 5000);
setInterval(applyLang, 6000);
</script>
</body>
</html>`
