// Real ModelTrace modules and HTML; only browser/transport boundaries are supplied.
// Requires Node >=20 and an existing happy-dom package with its dependencies.
// Run: node --experimental-vm-modules --test smart-router/modeltrace_runtime/tests/test_ui_recovery.js
// Defaults to repository web/node_modules; MODELTRACE_UI_WEB_DEPS may select any
// local dependency directory containing happy-dom. No installation is performed.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const vm = require("node:vm");
const { webcrypto } = require("node:crypto");
const { pathToFileURL } = require("node:url");

const root = path.resolve(__dirname, "..");
const deps = process.env.MODELTRACE_UI_WEB_DEPS || path.resolve(root, "../../web/node_modules");
const binding = { binding_id: "owned-binding", claimed_model: "GPT-4o", protocol: "chat" };
const bank = {
  generation: "generation-1", revision: "a".repeat(64), model_count: 1,
  families: [{ id: "custom", label: "Custom", row_count: 0, active: false }],
};
const analysis = {
  prediction: "GPT-4o", probability: 0.75, used_outputs: 1, complete: false,
  results: [{ model: "GPT-4o", family: "gpt", probability: 0.75 }],
};
const sample = { challenge_id: "owned-slot", expected_count: 100, numbers: Array(100).fill(17) };
const row = { ...sample, row_id: "row-1", strict_valid: true };
const committed = {
  status: "succeeded", counts: { requested: 1, transport_calls: 1, unknown: 0 },
  slots: { [sample.challenge_id]: { status: "done", result: { ...sample, eligible: true, row } } },
};
const tick = () => new Promise((resolve) => setImmediate(resolve));
const json = (value, status = 200) => new Response(JSON.stringify(value), { status });
const rejection = (code) => json({ error: code }, code === "invalid_request" ? 400 : 409);

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

async function until(predicate, description) {
  for (let turn = 0; turn < 200; turn += 1) {
    if (predicate()) return;
    await tick();
  }
  assert.fail(`UI did not reach: ${description}`);
}

async function harness(t, surface = "operator", storage = new Map(), initialRoutes = new Map()) {
  const realDeps = await fs.realpath(deps);
  const domPackage = path.join(realDeps, "happy-dom");
  const metadata = JSON.parse(await fs.readFile(path.join(domPackage, "package.json"), "utf8"));
  assert.equal(metadata.name, "happy-dom");
  const domEntry = await fs.realpath(require.resolve(domPackage));
  const { Window } = await import(pathToFileURL(domEntry).href);
  const window = new Window({
    url: surface === "operator" ? "http://modeltrace.invalid/" : "http://modeltrace.invalid/static-manual/",
    settings: { disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableJavaScriptEvaluation: true },
  });
  t.after(() => window.happyDOM.abort());
  const directory = surface === "operator" ? "web" : "static-manual";
  window.document.write(await fs.readFile(path.join(root, directory, "index.html"), "utf8"));
  const document = window.document;
  const calls = [], routes = new Map(initialRoutes), errors = [];
  const byId = (id) => {
    const node = document.getElementById(id);
    assert.ok(node, `real HTML must expose #${id}`);
    return node;
  };
  const fetch = async (url, options = {}) => {
    const request = {
      url: String(url), method: options.method || "GET",
      body: options.body ? JSON.parse(options.body) : undefined,
      lookupEnabled: !document.getElementById("lookup-operation")?.disabled,
      enrollmentLookupEnabled: Boolean(document.getElementById("lookup-enrollment")) &&
        !document.getElementById("lookup-enrollment").disabled,
      collectionText: document.getElementById("collection-status")?.textContent,
      enrollmentText: document.getElementById("enrollment-status")?.textContent,
    };
    calls.push(request);
    const handler = routes.get(`${request.method} ${request.url}`);
    if (handler) return handler(request);
    if (request.url === "/api/session") return json({ csrf_token: "test-only-csrf", bindings: [binding] });
    if (request.url === "/api/banks" && request.method === "GET") return json(bank);
    if (request.url === "/api/challenges") return json({
      challenge_set_id: "owned-plan",
      challenges: Array.from({ length: 3 }, (_, i) => ({ id: `challenge-${i}`, expected_count: 100, prompt: "Owned prompt" })),
    });
    if (request.url === "./data/unified_bank.json") {
      return new Response(await fs.readFile(path.join(root, "static-manual/data/unified_bank.json")));
    }
    if (request.url === "/api/analyze") return json({ analysis, bank_revision: bank.revision, samples: [sample] });
    if (request.url === "/api/banks/reload") return json({ summary: bank });
    errors.push(`unexpected transport: ${request.method} ${request.url}`);
    throw new Error("unexpected test transport");
  };
  const context = vm.createContext({
    document, window, fetch, crypto: webcrypto, TextEncoder, TextDecoder, Blob, URL,
    Uint8Array, Uint32Array,
    navigator: { clipboard: { writeText: async () => {} } },
    sessionStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, String(value)),
      removeItem: (key) => storage.delete(key),
    },
    console: { error: (...values) => errors.push(values.join(" ")), log: () => {} },
  });
  const modules = new Map();
  async function load(filename) {
    if (modules.has(filename)) return modules.get(filename);
    const module = new vm.SourceTextModule(await fs.readFile(filename, "utf8"), { context, identifier: filename });
    modules.set(filename, module);
    await module.link((specifier, parent) => {
      assert.ok(specifier.startsWith("./"), "only real local manual imports are permitted");
      return load(path.resolve(path.dirname(parent.identifier), specifier));
    });
    return module;
  }
  const module = await load(path.join(root, directory, surface === "operator" ? "app.js" : "pages-app.js"));
  await module.evaluate();
  await until(() => byId("binding-select").options.length === 1 &&
    (surface === "manual" || !byId("test-message").classList.contains("working")), "initialized real page");
  t.after(() => assert.deepEqual(errors, [], "no unexpected transport or console errors"));
  return {
    byId, calls, routes, storage,
    posts: (url) => calls.filter((item) => item.method === "POST" && (!url || item.url === url)),
    click: (id) => byId(id).click(),
    dispatch: (id) => byId(id).dispatchEvent(new window.Event("click", { bubbles: true })),
    submit: (id) => byId(id).dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true })),
    idle: async () => {
      if (surface === "operator") {
        await until(() => !byId("test-message").classList.contains("working"), "operator action settled");
      } else {
        await tick();
      }
    },
  };
}

function target(h, enrollment = false) {
  const prefix = enrollment ? "enroll" : "test-api";
  h.byId(`${prefix}-base`).value = "https://provider.invalid";
  h.byId(`${prefix}-model`).value = "provider-model";
  h.byId(`${prefix}-key`).value = "test-only-provider-secret";
  h.byId(enrollment ? "enroll-label" : "test-label").value = binding.claimed_model;
  h.byId(enrollment ? "enroll-protocol" : "test-protocol").value = "openai";
  if (enrollment) h.byId("enroll-family").value = "custom";
}

async function localAnalysis(h) {
  for (let index = 0; index < 3; index += 1) {
    const textarea = h.byId(`output-${index}`);
    const count = Number(textarea.closest(".challenge-item").querySelector(".challenge-header span").textContent.match(/\d+/)[0]);
    textarea.value = Array(count).fill(17).join(", ");
  }
  h.click("analyze");
  await h.idle();
  assert.equal(h.byId("result").hidden, false, "real local analysis renders");
}

async function lostCollection(h, enrollment = false) {
  target(h, enrollment);
  const endpoint = enrollment ? "/api/enroll/auto" : "/api/test/auto";
  h.routes.set(`POST ${endpoint}`, () => { throw new Error("lost response"); });
  h.submit(enrollment ? "auto-enrollment" : "api-test-form");
  await h.idle();
  return h.posts(endpoint)[0].body.operation_id;
}

for (const [name, value, valid] of [
  ["uppercase and underscore rejected", "UPPER_BAD", false],
  ["literal hyphen accepted", "custom-2", true],
  ["leading hyphen rejected", "-custom", false],
  ["64 characters accepted", "a".repeat(64), true],
  ["65 characters rejected", "a".repeat(65), false],
]) {
  test(`B1 family ID: ${name}`, async (t) => {
    const h = await harness(t);
    const input = h.byId("new-bank-id");
    // happy-dom does not compile patterns with HTML's v flag; check it explicitly.
    const pattern = new RegExp(`^(?:${input.pattern})$`, "v");
    input.value = value;
    assert.equal(pattern.test(value), valid);
    assert.equal(input.validity.patternMismatch, !valid);
    assert.equal(input.checkValidity(), valid);
    assert.equal(h.posts().length, 0, "native constraint checks must not publish");
  });
}

for (const enrollment of [false, true]) {
  const name = enrollment ? "enrollment" : "test";
  test(`F1 ${name}: reference and owned lookup exist before deferred POST finishes`, async (t) => {
    const h = await harness(t);
    const response = deferred();
    t.after(() => response.reject(new Error("lost response")));
    target(h, enrollment);
    const endpoint = enrollment ? "/api/enroll/auto" : "/api/test/auto";
    h.routes.set(`POST ${endpoint}`, () => response.promise);
    h.submit(enrollment ? "auto-enrollment" : "api-test-form");
    const post = h.posts(endpoint)[0];
    assert.ok(post);
    assert.equal(enrollment ? post.enrollmentLookupEnabled : post.lookupEnabled, true);
    assert.match(enrollment ? post.enrollmentText : post.collectionText, new RegExp(post.body.operation_id));
    assert.match(enrollment ? post.enrollmentText : post.collectionText, /chat.*GPT-4o|GPT-4o.*chat/);
    assert.equal(h.byId(enrollment ? "enroll-key" : "test-api-key").value, "");
    assert.ok([...h.storage.values()].some((value) => value.includes(post.body.operation_id)));
    assert.doesNotMatch([...h.storage.values()].join(""), /test-only-provider-secret|provider\.invalid|provider-model|test-only-csrf|numbers|prompt/);
    response.reject(new Error("lost response"));
    await h.idle();
    assert.equal(h.byId(enrollment ? "lookup-enrollment" : "lookup-operation").disabled, false);
    target(h, !enrollment);
    h.submit(enrollment ? "api-test-form" : "auto-enrollment");
    assert.equal(h.posts().length, 1, "unknown collection fences both collection forms");
  });

  test(`F1 ${name}: owned GET restores committed slots after lost response, without POST replay`, async (t) => {
    const h = await harness(t);
    const id = await lostCollection(h, enrollment);
    h.routes.set(`GET /api/operations/${id}`, () => json(committed));
    const lookup = enrollment ? "lookup-enrollment" : "lookup-operation";
    assert.equal(h.byId(lookup).disabled, false, "lost response must leave recovery usable");
    h.click(lookup);
    await h.idle();
    assert.match(h.byId(enrollment ? "enrollment-status" : "collection-status").textContent, new RegExp(id));
    if (enrollment) {
      assert.equal(h.byId("publish-enrollment").disabled, false, "slot rows restore publication reference");
      h.routes.set("POST /api/banks/custom/enroll", () => json({ summary: bank }));
      h.click("publish-enrollment");
      await h.idle();
      assert.equal(h.posts("/api/banks/custom/enroll")[0].body.operation_id, id);
      h.click(lookup);
      await h.idle();
      assert.equal(h.byId("publish-enrollment").disabled, true);
      assert.match(h.byId("enrollment-status").textContent, /Explicitly published/);
      assert.doesNotMatch(h.byId("enrollment-status").textContent, /Not published/);
    } else {
      assert.match(h.byId("collection-status").textContent, /requested 1/);
      assert.equal(h.byId("analyze-collected").disabled, false);
      h.click("analyze-collected");
      await h.idle();
      const analyzed = h.posts("/api/analyze")[0].body;
      assert.equal(analyzed.operation_id, id);
      assert.deepEqual(analyzed.samples, [sample]);
      assert.equal(h.byId("submit-evidence").disabled, false, "saved protocol/model selects the existing binding");
    }
    assert.equal(h.posts(enrollment ? "/api/enroll/auto" : "/api/test/auto").length, 1);
  });

  test(`F1 ${name}: safe session reference survives reload and unknown GET remains fenced`, async (t) => {
    const h = await harness(t);
    const id = await lostCollection(h, enrollment);
    const reloaded = await harness(t, "operator", h.storage);
    const lookup = enrollment ? "lookup-enrollment" : "lookup-operation";
    assert.equal(reloaded.byId(lookup).disabled, false);
    assert.match(reloaded.byId(enrollment ? "enrollment-status" : "collection-status").textContent, new RegExp(id));
    reloaded.routes.set(`GET /api/operations/${id}`, () => json({
      ...committed, status: "write_unknown", counts: { ...committed.counts, unknown: 1, transport_calls: null },
    }));
    reloaded.click(lookup);
    await reloaded.idle();
    target(reloaded);
    reloaded.submit("api-test-form");
    target(reloaded, true);
    reloaded.submit("auto-enrollment");
    assert.equal(reloaded.posts().length, 0, "unknown readback does not unlock a fresh provider operation");
    assert.match(reloaded.byId(enrollment ? "enrollment-status" : "collection-status").textContent, /unknown/);
  });
}

test("F1 startup failure: saved operation remains recoverable when bank initialization fails", async (t) => {
  const h = await harness(t);
  const id = await lostCollection(h);
  const reloaded = await harness(t, "operator", h.storage, new Map([
    ["GET /api/banks", () => rejection("restart_required")],
  ]));
  assert.match(reloaded.byId("test-message").textContent, /restart_required/);
  assert.equal(reloaded.byId("lookup-operation").disabled, false);
  assert.match(reloaded.byId("collection-status").textContent, new RegExp(id));
  reloaded.routes.set(`GET /api/operations/${id}`, () => json(committed));
  reloaded.click("lookup-operation");
  await reloaded.idle();
  assert.equal(reloaded.byId("analyze-collected").disabled, false);
  assert.equal(reloaded.posts().length, 0);
});

for (const surface of ["operator", "manual"]) {
  test(`F2 ${surface}: unknown export retains original ID and signed readback through regeneration/reanalysis`, async (t) => {
    const h = await harness(t, surface);
    await localAnalysis(h);
    h.routes.set("POST /api/export", () => { throw new Error("lost export response"); });
    h.click("submit-evidence");
    await h.idle();
    const id = h.posts("/api/export")[0].body.evidence.probe_id;
    const unknown = h.byId("export-status").textContent;
    h.click("regenerate");
    await h.idle();
    assert.equal(h.byId("probe-id").value, id);
    assert.equal(h.byId("read-probe").disabled, false);
    await localAnalysis(h);
    assert.equal(h.byId("probe-id").value, id);
    assert.equal(h.byId("export-status").textContent, unknown);
    assert.equal(h.byId("submit-evidence").disabled, true);
    h.dispatch("submit-evidence");
    await h.idle();
    assert.equal(h.posts("/api/export").length, 1);
    h.routes.set(`GET /api/probes/${id}`, () => json({ probe_id: id, state: "running" }));
    h.click("read-probe");
    await h.idle();
    assert.ok(h.calls.some((item) => item.url === `/api/probes/${id}` && item.method === "GET"));
    assert.match(h.byId("export-status").textContent, new RegExp(id));
  });
}

test("F2 manual: inflight export fences double submit and late receipt stays attached to original probe", async (t) => {
  const h = await harness(t, "manual");
  await localAnalysis(h);
  const response = deferred();
  t.after(() => response.reject(new Error("lost response")));
  h.routes.set("POST /api/export", () => response.promise);
  h.click("submit-evidence");
  const id = h.posts("/api/export")[0].body.evidence.probe_id;
  h.click("regenerate");
  await localAnalysis(h);
  h.dispatch("submit-evidence");
  assert.equal(h.posts("/api/export").length, 1, "new analysis must not admit a concurrent export");
  assert.equal(h.byId("probe-id").value, id);
  response.resolve(json({ probe_id: id, receipt: "original-receipt" }));
  await until(() => h.byId("export-status").textContent.includes("original-receipt"), "original late receipt rendered");
  assert.equal(h.byId("probe-id").value, id);
  assert.equal(h.byId("read-probe").disabled, false);
  h.click("regenerate");
  assert.match(h.byId("export-status").textContent, /original-receipt/);
});

test("F2 manual: late export response cannot overwrite newer readback for the same pinned probe", async (t) => {
  const h = await harness(t, "manual");
  await localAnalysis(h);
  const response = deferred();
  h.routes.set("POST /api/export", () => response.promise);
  h.click("submit-evidence");
  const id = h.posts("/api/export")[0].body.evidence.probe_id;
  h.routes.set(`GET /api/probes/${id}`, () => json({ probe_id: id, state: "succeeded", receipt: "signed-readback" }));
  h.click("read-probe");
  await until(() => h.byId("export-status").textContent.includes("signed-readback"), "signed readback");
  response.resolve(json({ probe_id: id, receipt: "older-submit-receipt" }));
  await tick();
  assert.match(h.byId("export-status").textContent, /signed-readback/);
  assert.doesNotMatch(h.byId("export-status").textContent, /older-submit-receipt/);
});

async function completedEnrollment(h) {
  target(h, true);
  h.routes.set("POST /api/enroll/auto", () => json({ ...committed, rows: [row] }));
  h.submit("auto-enrollment");
  await h.idle();
  return h.posts("/api/enroll/auto")[0].body.operation_id;
}

for (const failure of ["transport", "parse", "server"]) {
  test(`I2 enrollment: confirmed POST survives ${failure} bank refresh failure without republishing`, async (t) => {
    const h = await harness(t);
    const id = await completedEnrollment(h);
    const endpoint = "/api/banks/custom/enroll";
    const nextBank = { ...bank, generation: "generation-2", revision: "b".repeat(64) };
    const refresh = deferred();
    t.after(() => refresh.reject(new Error("test cleanup")));
    h.routes.set(`POST ${endpoint}`, () => json({ summary: nextBank }));
    h.routes.set("GET /api/banks", () => refresh.promise);
    const callsBeforePublication = h.calls.length;
    h.click("publish-enrollment");
    await until(() => h.calls.slice(callsBeforePublication).some((call) =>
      call.method === "GET" && call.url === "/api/banks"), "post-publication bank refresh started");
    const statusBeforeRefresh = h.calls.at(-1).enrollmentText;
    if (failure === "transport") refresh.reject(new Error("bank refresh lost"));
    else if (failure === "parse") refresh.resolve(new Response("{broken", { status: 200 }));
    else refresh.resolve(json({ error: "unavailable" }, 503));
    await h.idle();
    const statusAfterFailure = h.byId("enrollment-status").textContent;
    const messageAfterFailure = h.byId("test-message").textContent;
    h.click("publish-enrollment");
    await h.idle();
    const postsAfterFailureClick = h.posts(endpoint).length;

    h.routes.set(`GET /api/operations/${id}`, () => json(committed));
    h.click("lookup-enrollment");
    await h.idle();
    const disabledAfterLookup = h.byId("publish-enrollment").disabled;
    const statusAfterLookup = h.byId("enrollment-status").textContent;
    h.routes.set("GET /api/banks", () => json(nextBank));
    h.click("reload");
    await h.idle();
    assert.match(h.byId("bank-revision").textContent, /generation-2/);
    h.click("publish-enrollment");
    await h.idle();
    assert.equal(h.posts(endpoint).length, 1, "owned lookup and refresh must not admit a second publication POST");
    assert.equal(postsAfterFailureClick, 1);
    assert.equal(disabledAfterLookup, true, "owned collection GET cannot undo a confirmed publication");
    assert.equal(h.byId("publish-enrollment").disabled, true);
    for (const status of [statusBeforeRefresh, statusAfterFailure, statusAfterLookup,
      h.byId("enrollment-status").textContent]) {
      assert.match(status, new RegExp(id));
      assert.match(status, /Explicitly published/);
      assert.doesNotMatch(status, /Not published/);
    }
    assert.match(messageAfterFailure, /Publication confirmed.*refresh/i);
    assert.doesNotMatch(messageAfterFailure, /write_unknown|No action was retried/);
    h.dispatch("publish-enrollment");
    await h.idle();
    assert.equal(h.posts(endpoint).length, 1, "the callback also guards an already-published enrollment");
    assert.match(h.byId("test-message").textContent, /already published.*No POST/i);
    assert.equal(h.posts("/api/enroll/auto").length, 1, "no new collection occurred");
    assert.equal(h.posts(endpoint)[0].body.operation_id, id);
  });
}

for (const action of ["create", "enroll", "rebuild"]) {
  for (const code of ["invalid_request", "stale_bank"]) {
    test(`F3 ${action}: authoritative ${code} permits correction while preserving enrollment`, async (t) => {
      const h = await harness(t);
      const id = await completedEnrollment(h);
      const endpoint = action === "create" ? "/api/banks" :
        action === "enroll" ? "/api/banks/custom/enroll" : "/api/banks/rebuild";
      h.byId("new-bank-id").value = "gpt";
      h.byId("new-bank-name").value = "Reserved family";
      h.routes.set(`POST ${endpoint}`, () => rejection(code));
      const trigger = () => action === "create" ? h.submit("create-bank-form") :
        h.click(action === "enroll" ? "publish-enrollment" : "rebuild");
      trigger();
      await h.idle();
      assert.match(h.byId("test-message").textContent, new RegExp(code));
      const nextBank = { ...bank, generation: "generation-2", revision: "b".repeat(64) };
      h.routes.set("GET /api/banks", () => json(nextBank));
      // A second stale response should refresh to the new snapshot before correction.
      if (code === "stale_bank") {
        trigger();
        await h.idle();
        assert.match(h.byId("bank-revision").textContent, /generation-2/);
      }
      h.routes.set(`POST ${endpoint}`, () => json({ summary: nextBank }));
      h.byId("new-bank-id").value = "custom-2";
      trigger();
      await h.idle();
      assert.equal(h.posts(endpoint).length, code === "stale_bank" ? 3 : 2);
      const last = h.posts(endpoint).at(-1).body;
      if (code === "stale_bank") assert.equal(last.generation, "generation-2");
      if (action === "enroll") assert.equal(last.operation_id, id);
      assert.match(h.byId("enrollment-status").textContent, new RegExp(id));
    });
  }
}

for (const failure of ["transport", "parse", "write_unknown", "restart_required", "local-invalid", "non-authoritative-invalid"]) {
  test(`F3 ${failure}: uncertain publication stays fenced and blocked no-op never reports success`, async (t) => {
    const h = await harness(t);
    await completedEnrollment(h);
    h.byId("new-bank-id").value = "custom-2";
    h.byId("new-bank-name").value = failure === "local-invalid" ? "x".repeat(270000) : "Custom 2";
    h.routes.set("POST /api/banks", () => {
      if (failure === "transport") throw new Error("lost response");
      if (failure === "parse") return new Response("{broken", { status: 200 });
      if (failure === "non-authoritative-invalid") return json({ error: "invalid_request" });
      return rejection(failure);
    });
    h.submit("create-bank-form");
    await h.idle();
    const postCount = h.posts().length;
    h.byId("new-bank-name").value = "Corrected";
    h.click("reload");
    await h.idle();
    const afterReload = h.posts().length;
    for (const blocked of ["rebuild", "publish-enrollment"]) {
      h.dispatch(blocked);
      await h.idle();
      assert.equal(h.posts().length, afterReload);
      assert.doesNotMatch(h.byId("test-message").textContent, /Action finished/);
      assert.match(h.byId("test-message").textContent, /block|unknown|restart/i);
    }
    h.submit("create-bank-form");
    await h.idle();
    assert.equal(h.posts().length, postCount + 1, "only the explicit reload POST was sent");
  });
}
