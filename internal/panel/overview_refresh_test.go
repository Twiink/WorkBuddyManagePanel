package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOverviewRefreshRecoversFromFailedAndStalledReads(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const src = fs.readFileSync(process.argv[2], 'utf8');
const from = src.indexOf('function renderOverviewFreshness()');
const to = src.indexOf('function renderOverview(d)', from);
assert.ok(from >= 0 && to > from);
const status = {textContent:'', style:{}};
const button = {};
const requests = [], renders = [], timers = new Map(), errors = [];
let timerID = 0;
const ctx = {
  $: id => id === 'overviewFreshness' ? status : id === 'btnRefreshOverview' ? button : null,
  ago: () => '1 分钟前',
  AbortController, Date, Array, Object, console: {error: (...args) => errors.push(args)},
  setTimeout: callback => { timers.set(++timerID, callback); return timerID; },
  clearTimeout: id => timers.delete(id),
  packageCacheMatchesAccounts: () => true,
  clearPackageCache() {}, toast() {},
  renderOverview: data => renders.push(data),
  api: (path, opts) => new Promise((resolve, reject) => {
    assert.equal(path, 'overview');
    requests.push({opts, resolve, reject});
    opts.signal.addEventListener('abort', () => reject(new Error('aborted')), {once:true});
  }),
};
vm.createContext(ctx);
vm.runInContext('let overviewRequest=null, overviewData=null, overviewUpdatedAt=0, overviewError="";\n' + src.slice(from,to),ctx);
const snapshot = version => ({version,accounts:[],model_locks:version===2?[{model:'kimi-k3.1-pro',locked:3}]:[]});
(async () => {
  const first = ctx.loadOverview(true);
  assert.equal(first, ctx.loadOverview(true), 'overlapping refreshes must share one request');
  assert.equal(requests.length,1);
  assert.equal(requests[0].opts.cache,'no-store');
  requests[0].resolve(snapshot(1)); await first;
  assert.equal(renders.length,1);
  assert.match(status.textContent,/状态已更新/);
  const failed = ctx.loadOverview(true);
  requests[1].reject(new Error('network failed')); await failed;
  assert.equal(renders.length,1,'failed read must retain last rendered snapshot');
  assert.match(status.textContent,/数据未更新.*刷新失败/);
  const stalled = ctx.loadOverview(true);
  assert.equal(timers.size,1);
  [...timers.values()][0](); await stalled;
  assert.match(status.textContent,/刷新超时/);
  assert.equal(renders.length,1);
  assert.equal(timers.size,0);
  const recovered = ctx.loadOverview(true);
  requests[3].resolve(snapshot(2)); await recovered;
  assert.equal(renders.at(-1).model_locks[0].locked,3);
  assert.match(status.textContent,/状态已更新/);
  assert.doesNotMatch(status.textContent,/未更新|失败|超时/);
  const invalid = ctx.loadOverview(true);
  requests[4].resolve({}); await invalid;
  assert.equal(renders.length,2,'invalid response must not masquerade as an empty healthy pool');
  assert.match(status.textContent,/数据未更新/);
  assert.equal(errors.length,3);
  console.log('overview refresh recovery OK');
})().catch(e => { console.error(e); process.exitCode=1; });`
	path := filepath.Join(t.TempDir(), "overview-refresh.cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("refresh regression failed: %v\n%s", err, output)
	}
}
