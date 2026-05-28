const state = {
  adminSecret: sessionStorage.getItem("redeemer_admin_secret") || "",
  plans: [],
  plansById: new Map(),
  plansLoaded: false,
  selectedCodes: new Set(),
  visibleCodes: [],
  review: null,
  toastTimer: null,
};

const page = document.body.dataset.page || "redeem";
const $ = (selector) => document.querySelector(selector);
const auditEventLabels = {
  "codes.created": "生成兑换码",
  "code.status_changed": "兑换码状态变更",
  "codes.status_changed": "批量状态变更",
  "codes.deleted": "批量删除",
  "code.redeemed": "兑换成功",
  "code.redeem_failed": "兑换失败",
};

function currentAdminApiBase() {
  const marker = "/admin";
  const markerIndex = window.location.pathname.lastIndexOf(marker);
  const prefix = markerIndex > 0 ? window.location.pathname.slice(0, markerIndex) : "";
  return `${prefix}/api/v1/admin`;
}

function setHealth(ok, message) {
  const dot = $("#health-dot");
  const text = $("#health-text");
  if (!dot || !text) {
    return;
  }
  dot.classList.toggle("is-ok", ok);
  dot.classList.toggle("is-bad", !ok);
  text.textContent = message;
}

function setResult(node, ok, message) {
  if (!node) {
    return;
  }
  node.hidden = false;
  node.classList.toggle("is-ok", ok);
  node.classList.toggle("is-error", !ok);
  node.textContent = message;
}

function showToast(message) {
  const toast = $("#toast");
  if (!toast) {
    return;
  }
  toast.textContent = message;
  toast.hidden = false;
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => {
    toast.hidden = true;
  }, 3200);
}

async function apiJson(path, options = {}) {
  const headers = {
    Accept: "application/json",
    ...(options.headers || {}),
  };
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (options.admin) {
    if (!state.adminSecret) {
      throw new Error("请先填写管理密钥");
    }
    headers["X-Admin-Secret"] = state.adminSecret;
  }

  const response = await fetch(path, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok || payload.success === false) {
    throw new Error(payload.message || `请求失败: HTTP ${response.status}`);
  }
  return payload;
}

function localDatetimeToIso(value) {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    throw new Error("过期时间格式不正确");
  }
  return date.toISOString();
}

function formatTime(value) {
  if (!value) {
    return "未设置";
  }
  return new Date(value * 1000).toLocaleString();
}

function redeemPayloadFromForm() {
  return {
    code: $("#redeem-code").value.trim(),
    user_id: Number($("#redeem-user-id").value),
    email: $("#redeem-email").value.trim(),
  };
}

function clearReview() {
  state.review = null;
  const panel = $("#review-panel");
  if (panel) {
    panel.hidden = true;
  }
}

function fillReview(data) {
  const planName = data.plan_name || data.plan?.title || data.plan?.name || "";
  state.review = {
    code: data.code,
    user_id: data.user_id,
    email: data.user?.email || data.email || "",
    plan_id: data.plan_id,
    plan_name: planName,
  };
  const subscription = data.user?.subscription;
  $("#review-code").textContent = data.code;
  $("#review-user-id").textContent = String(data.user_id);
  $("#review-username").textContent = data.user?.username || "-";
  $("#review-email").textContent = data.user?.email || "-";
  $("#review-group").textContent = data.user?.group || "-";
  $("#review-subscription").textContent = subscription ? JSON.stringify(subscription) : "未返回";
  $("#review-plan-name").textContent = planName || "未返回";
  $("#review-expires-at").textContent = formatTime(data.expires_at);
  $("#review-panel").hidden = false;
}

function bindRedeemPage() {
  const form = $("#review-form");
  const result = $("#redeem-result");

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    result.hidden = true;
    clearReview();
    try {
      const payload = await apiJson("/api/v1/redeem/preview", {
        method: "POST",
        body: redeemPayloadFromForm(),
      });
      fillReview(payload.data);
      showToast(payload.message || "兑换信息可用");
    } catch (error) {
      setResult(result, false, error.message);
    }
  });

  $("#confirm-redeem").addEventListener("click", async () => {
    if (!state.review) {
      showToast("请先核对兑换信息");
      return;
    }
    result.hidden = true;
    const review = state.review;
    try {
      const payload = await apiJson("/api/v1/redeem", {
        method: "POST",
        body: review,
      });
      clearReview();
      const planSuffix = review?.plan_name ? `：${review.plan_name}` : "";
      setResult(result, true, `${payload.message || "订阅已激活"}${planSuffix}`);
    } catch (error) {
      setResult(result, false, error.message);
    }
  });

  $("#edit-review").addEventListener("click", clearReview);
  $("#redeem-code").addEventListener("input", clearReview);
  $("#redeem-user-id").addEventListener("input", clearReview);
  $("#redeem-email").addEventListener("input", clearReview);
}

function syncSecretField() {
  const input = $("#admin-secret");
  const label = $("#secret-state");
  if (!input || !label) {
    return;
  }
  input.value = state.adminSecret;
  label.textContent = state.adminSecret ? "管理密钥已保存在当前浏览器会话中。" : "密钥只保存在当前浏览器会话中。";
}

function planIdOf(plan) {
  const id = Number(plan?.id ?? plan?.plan_id);
  return Number.isFinite(id) && id > 0 ? id : null;
}

function normalizeBool(value) {
  if (typeof value === "boolean") {
    return value;
  }
  if (typeof value === "number") {
    return value !== 0;
  }
  if (typeof value === "string") {
    const text = value.trim().toLowerCase();
    if (["1", "true", "enabled", "active"].includes(text)) {
      return true;
    }
    if (["0", "false", "disabled", "inactive"].includes(text)) {
      return false;
    }
  }
  return null;
}

function planEnabled(plan) {
  return normalizeBool(plan?.enabled ?? plan?.enable ?? plan?.is_enabled);
}

function planStatusText(plan) {
  const enabled = planEnabled(plan);
  if (enabled === true) {
    return "启用";
  }
  if (enabled === false) {
    return "停用";
  }
  return "状态未知";
}

function planName(plan) {
  const id = planIdOf(plan);
  return plan?.plan_name || plan?.title || plan?.name || plan?.display_name || (id ? `套餐 ${id}` : "未知套餐");
}

function planLabel(plan) {
  return `${planName(plan)}（${planStatusText(plan)}）`;
}

function planLabelById(planId) {
  const id = Number(planId);
  const plan = state.plansById.get(String(id));
  return plan ? planLabel(plan) : `套餐 ${id}`;
}

function auditEventLabel(eventType) {
  return auditEventLabels[eventType] || eventType || "-";
}

function auditRedeemUser(item) {
  const metadata = item.metadata || {};
  const userId = metadata.redeem_user_id || metadata.user_id || "";
  const username = metadata.redeem_username || metadata.username || "";
  const email = metadata.redeem_email || metadata.email || "";
  const label = username && userId ? `${username} (${userId})` : username || (userId ? `ID ${userId}` : "");
  return { label, email };
}

function resetPlans() {
  state.plans = [];
  state.plansById = new Map();
  state.plansLoaded = false;
  renderPlanSelects();
}

function renderPlanSelects() {
  const createSelect = $("#create-plan-id");
  const filterSelect = $("#filter-plan-id");
  if (!createSelect || !filterSelect) {
    return;
  }

  const createCurrent = createSelect.value;
  const filterCurrent = filterSelect.value;
  const createPlaceholderText = state.plansLoaded && state.plans.length === 0
    ? "没有可用套餐"
    : state.plansLoaded ? "选择套餐" : "先填写管理密钥加载套餐";
  const createPlaceholder = new Option(createPlaceholderText, "");
  createPlaceholder.disabled = true;
  const filterPlaceholder = new Option("全部套餐", "");
  const planOptions = state.plans.map((plan) => new Option(planLabel(plan), String(planIdOf(plan))));

  createSelect.replaceChildren(createPlaceholder, ...planOptions.map((option) => option.cloneNode(true)));
  filterSelect.replaceChildren(filterPlaceholder, ...planOptions);
  createSelect.value = Array.from(createSelect.options).some((option) => option.value === createCurrent) ? createCurrent : "";
  filterSelect.value = Array.from(filterSelect.options).some((option) => option.value === filterCurrent) ? filterCurrent : "";
}

async function loadPlans(options = {}) {
  if (!state.adminSecret) {
    resetPlans();
    return [];
  }
  if (state.plansLoaded && !options.force) {
    return state.plans;
  }
  const payload = await apiJson(`${currentAdminApiBase()}/plans`, { admin: true });
  state.plans = Array.isArray(payload.data) ? payload.data : [];
  state.plansById = new Map(
    state.plans
      .map((plan) => [String(planIdOf(plan)), plan])
      .filter(([id]) => id !== "null"),
  );
  state.plansLoaded = true;
  renderPlanSelects();
  return state.plans;
}

function describeCode(item) {
  const parts = [
    planLabelById(item.plan_id),
    `创建 ${formatTime(item.created_at)}`,
  ];
  if (item.expires_at) {
    parts.push(`过期 ${formatTime(item.expires_at)}`);
  }
  if (item.used_by_user_id) {
    parts.push(`用户 ${item.used_by_user_id}`);
  }
  if (item.note) {
    parts.push(item.note);
  }
  if (item.last_error) {
    parts.push(`错误: ${item.last_error}`);
  }
  return parts;
}

function selectedCodeList() {
  return Array.from(state.selectedCodes);
}

function updateBulkControls() {
  const selectedCount = state.selectedCodes.size;
  const summary = $("#selected-summary");
  if (summary) {
    summary.textContent = `已选 ${selectedCount}`;
  }
  for (const selector of ["#bulk-disable", "#bulk-restore", "#bulk-delete"]) {
    const button = $(selector);
    if (button) {
      button.disabled = selectedCount === 0;
    }
  }
  const selectVisible = $("#select-visible-codes");
  if (selectVisible) {
    const visibleCount = state.visibleCodes.length;
    const selectedVisibleCount = state.visibleCodes.filter((code) => state.selectedCodes.has(code)).length;
    selectVisible.checked = visibleCount > 0 && selectedVisibleCount === visibleCount;
    selectVisible.indeterminate = selectedVisibleCount > 0 && selectedVisibleCount < visibleCount;
    selectVisible.disabled = visibleCount === 0;
  }
}

function renderCodes(items) {
  const list = $("#codes-list");
  state.visibleCodes = items.map((item) => item.code);
  state.selectedCodes = new Set(selectedCodeList().filter((code) => state.visibleCodes.includes(code)));
  updateBulkControls();
  $("#list-summary").textContent = `共 ${items.length} 条`;
  if (!items.length) {
    list.innerHTML = '<div class="empty-state">没有匹配的兑换码。</div>';
    return;
  }

  list.replaceChildren(
    ...items.map((item) => {
      const card = document.createElement("div");
      card.className = "code-item selectable-code";

      const select = document.createElement("input");
      select.className = "code-select";
      select.type = "checkbox";
      select.dataset.action = "select-code";
      select.dataset.code = item.code;
      select.checked = state.selectedCodes.has(item.code);
      select.setAttribute("aria-label", `选择 ${item.code}`);

      const main = document.createElement("div");
      main.className = "code-main";

      const code = document.createElement("p");
      code.className = "code-text";
      code.textContent = item.code;

      const meta = document.createElement("p");
      meta.className = "code-meta";
      const chip = document.createElement("span");
      chip.className = `status-chip status-${item.status}`;
      chip.textContent = item.status;
      meta.append(chip);
      for (const text of describeCode(item)) {
        const span = document.createElement("span");
        span.textContent = text;
        meta.append(span);
      }
      main.append(code, meta);

      const actions = document.createElement("div");
      actions.className = "code-actions";

      const copy = document.createElement("button");
      copy.className = "mini-button";
      copy.type = "button";
      copy.dataset.action = "copy";
      copy.dataset.code = item.code;
      copy.textContent = "复制";
      actions.append(copy);

      if (item.status === "active" || item.status === "disabled") {
        const toggle = document.createElement("button");
        toggle.className = "mini-button";
        toggle.type = "button";
        toggle.dataset.action = "status";
        toggle.dataset.code = item.code;
        toggle.dataset.status = item.status === "active" ? "disabled" : "active";
        toggle.textContent = item.status === "active" ? "停用" : "恢复";
        actions.append(toggle);
      }

      card.append(select, main, actions);
      return card;
    }),
  );
}

function describeAuditEvent(item) {
  const parts = [
    `时间 ${formatTime(item.created_at)}`,
    `操作者 ${item.actor_type || "-"}:${item.actor_id || "-"}`,
  ];
  if (item.code) {
    parts.push(`兑换码 ${item.code}`);
  }
  if (item.plan_id) {
    parts.push(planLabelById(item.plan_id));
  }
  const redeemUser = auditRedeemUser(item);
  if (redeemUser.label) {
    parts.push(`用户 ${redeemUser.label}`);
  }
  if (redeemUser.email) {
    parts.push(`邮箱 ${redeemUser.email}`);
  }
  if (item.status) {
    parts.push(`状态 ${item.status}`);
  }
  if (item.message) {
    parts.push(item.message);
  }
  return parts;
}

function renderAuditEvents(items) {
  const list = $("#audit-list");
  $("#audit-summary").textContent = `共 ${items.length} 条`;
  if (!items.length) {
    list.innerHTML = '<div class="empty-state">没有匹配的审计日志。</div>';
    return;
  }

  list.replaceChildren(
    ...items.map((item) => {
      const card = document.createElement("div");
      card.className = "code-item";

      const main = document.createElement("div");
      main.className = "code-main";

      const title = document.createElement("p");
      title.className = "code-text";
      title.textContent = auditEventLabel(item.event_type);

      const meta = document.createElement("p");
      meta.className = "code-meta";
      const chip = document.createElement("span");
      chip.className = "status-chip event-chip";
      chip.textContent = `#${item.id}`;
      meta.append(chip);
      for (const text of describeAuditEvent(item)) {
        const span = document.createElement("span");
        span.textContent = text;
        meta.append(span);
      }
      main.append(title, meta);
      card.append(main);
      return card;
    }),
  );
}

async function refreshCodes() {
  await loadPlans();
  const status = $("#filter-status").value;
  const planId = $("#filter-plan-id").value.trim();
  const limit = $("#filter-limit").value.trim() || "50";
  const query = new URLSearchParams({ limit });
  if (status) {
    query.set("status", status);
  }
  if (planId) {
    query.set("plan_id", planId);
  }
  const payload = await apiJson(`${currentAdminApiBase()}/codes?${query.toString()}`, { admin: true });
  renderCodes(payload.data || []);
}

async function refreshAuditEvents() {
  await loadPlans();
  const eventType = $("#filter-event-type").value;
  const code = $("#filter-audit-code").value.trim();
  const limit = $("#filter-audit-limit").value.trim() || "50";
  const query = new URLSearchParams({ limit });
  if (eventType) {
    query.set("event_type", eventType);
  }
  if (code) {
    query.set("code", code);
  }
  const payload = await apiJson(`${currentAdminApiBase()}/audit-events?${query.toString()}`, { admin: true });
  renderAuditEvents(payload.data || []);
}

async function refreshAdminLists() {
  await refreshCodes();
  await refreshAuditEvents();
}

async function runBatchCodeAction(action) {
  const codes = selectedCodeList();
  if (!codes.length) {
    showToast("请先选择兑换码");
    return;
  }
  if (action === "delete" && !window.confirm(`确认删除选中的 ${codes.length} 个兑换码？`)) {
    return;
  }
  const payload = await apiJson(`${currentAdminApiBase()}/codes/batch`, {
    method: "POST",
    admin: true,
    body: { action, codes },
  });
  state.selectedCodes.clear();
  await refreshCodes();
  await refreshAuditEvents();
  const count = payload.data?.count ?? codes.length;
  const labels = {
    delete: "已删除",
    disable: "已停用",
    restore: "已恢复",
  };
  showToast(`${labels[action] || "已处理"} ${count} 个兑换码`);
}

function bindAdminPage() {
  syncSecretField();
  renderPlanSelects();
  if (state.adminSecret) {
    refreshAdminLists().catch((error) => showToast(error.message));
  }

  $("#secret-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    state.adminSecret = $("#admin-secret").value.trim();
    if (!state.adminSecret) {
      sessionStorage.removeItem("redeemer_admin_secret");
      resetPlans();
      syncSecretField();
      showToast("请先填写管理密钥");
      return;
    }
    sessionStorage.setItem("redeemer_admin_secret", state.adminSecret);
    syncSecretField();
    try {
      await loadPlans({ force: true });
      await refreshAdminLists();
      showToast(`已加载 ${state.plans.length} 个套餐`);
    } catch (error) {
      resetPlans();
      showToast(error.message);
    }
  });

  $("#clear-secret").addEventListener("click", () => {
    state.adminSecret = "";
    sessionStorage.removeItem("redeemer_admin_secret");
    resetPlans();
    syncSecretField();
    showToast("管理密钥已清除");
  });

  $("#create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const result = $("#create-result");
    result.hidden = true;
    try {
      await loadPlans();
      const payload = await apiJson(`${currentAdminApiBase()}/codes`, {
        method: "POST",
        admin: true,
        body: {
          plan_id: Number($("#create-plan-id").value),
          count: Number($("#create-count").value),
          prefix: $("#create-prefix").value.trim() || "SUB",
          note: $("#create-note").value.trim(),
          expires_at: localDatetimeToIso($("#create-expires-at").value),
        },
      });
      const codes = (payload.data || []).map((item) => item.code).join("\n");
      setResult(result, true, codes || "已生成");
      await refreshCodes();
      await refreshAuditEvents();
    } catch (error) {
      setResult(result, false, error.message);
    }
  });

  $("#refresh-codes").addEventListener("click", async () => {
    try {
      await refreshCodes();
    } catch (error) {
      showToast(error.message);
    }
  });

  $("#refresh-audit").addEventListener("click", async () => {
    try {
      await refreshAuditEvents();
    } catch (error) {
      showToast(error.message);
    }
  });

  for (const selector of ["#filter-status", "#filter-plan-id", "#filter-limit"]) {
    $(selector).addEventListener("change", async () => {
      try {
        await refreshCodes();
      } catch (error) {
        showToast(error.message);
      }
    });
  }

  for (const selector of ["#filter-event-type", "#filter-audit-code", "#filter-audit-limit"]) {
    $(selector).addEventListener("change", async () => {
      try {
        await refreshAuditEvents();
      } catch (error) {
        showToast(error.message);
      }
    });
  }

  $("#select-visible-codes").addEventListener("change", (event) => {
    if (event.target.checked) {
      for (const code of state.visibleCodes) {
        state.selectedCodes.add(code);
      }
    } else {
      for (const code of state.visibleCodes) {
        state.selectedCodes.delete(code);
      }
    }
    document.querySelectorAll("input[data-action='select-code']").forEach((input) => {
      input.checked = state.selectedCodes.has(input.dataset.code);
    });
    updateBulkControls();
  });

  for (const [selector, action] of [
    ["#bulk-disable", "disable"],
    ["#bulk-restore", "restore"],
    ["#bulk-delete", "delete"],
  ]) {
    $(selector).addEventListener("click", async () => {
      try {
        await runBatchCodeAction(action);
      } catch (error) {
        showToast(error.message);
      }
    });
  }

  $("#codes-list").addEventListener("click", async (event) => {
    const checkbox = event.target.closest("input[data-action='select-code']");
    if (checkbox) {
      if (checkbox.checked) {
        state.selectedCodes.add(checkbox.dataset.code);
      } else {
        state.selectedCodes.delete(checkbox.dataset.code);
      }
      updateBulkControls();
      return;
    }

    const button = event.target.closest("button[data-action]");
    if (!button) {
      return;
    }
    const code = button.dataset.code;
    if (button.dataset.action === "copy") {
      try {
        await navigator.clipboard.writeText(code);
        showToast("兑换码已复制");
      } catch (error) {
        showToast("复制失败，请手动选择兑换码");
      }
      return;
    }
    if (button.dataset.action === "status") {
      try {
        await apiJson(`${currentAdminApiBase()}/codes/status`, {
          method: "POST",
          admin: true,
          body: {
            code,
            status: button.dataset.status,
          },
        });
        await refreshCodes();
        await refreshAuditEvents();
        showToast("状态已更新");
      } catch (error) {
        showToast(error.message);
      }
    }
  });
}

async function checkHealth() {
  try {
    await apiJson("/healthz");
    setHealth(true, "服务在线");
  } catch (error) {
    setHealth(false, "服务离线");
  }
}

if (page === "admin") {
  bindAdminPage();
} else {
  bindRedeemPage();
}
checkHealth();
setInterval(checkHealth, 20000);
