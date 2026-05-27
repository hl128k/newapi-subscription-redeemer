const state = {
  adminSecret: sessionStorage.getItem("redeemer_admin_secret") || "",
  review: null,
  toastTimer: null,
};

const page = document.body.dataset.page || "redeem";
const $ = (selector) => document.querySelector(selector);

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
  state.review = {
    code: data.code,
    user_id: data.user_id,
  };
  $("#review-code").textContent = data.code;
  $("#review-user-id").textContent = String(data.user_id);
  $("#review-plan-id").textContent = String(data.plan_id);
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
    try {
      const payload = await apiJson("/api/v1/redeem", {
        method: "POST",
        body: state.review,
      });
      clearReview();
      setResult(result, true, payload.message || "订阅已激活");
    } catch (error) {
      setResult(result, false, error.message);
    }
  });

  $("#edit-review").addEventListener("click", clearReview);
  $("#redeem-code").addEventListener("input", clearReview);
  $("#redeem-user-id").addEventListener("input", clearReview);
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

function describeCode(item) {
  const parts = [
    `套餐 ${item.plan_id}`,
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

function renderCodes(items) {
  const list = $("#codes-list");
  $("#list-summary").textContent = `共 ${items.length} 条`;
  if (!items.length) {
    list.innerHTML = '<div class="empty-state">没有匹配的兑换码。</div>';
    return;
  }

  list.replaceChildren(
    ...items.map((item) => {
      const card = document.createElement("div");
      card.className = "code-item";

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

      card.append(main, actions);
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
    parts.push(`套餐 ${item.plan_id}`);
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
      title.textContent = item.event_type;

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

function bindAdminPage() {
  syncSecretField();

  $("#secret-form").addEventListener("submit", (event) => {
    event.preventDefault();
    state.adminSecret = $("#admin-secret").value.trim();
    sessionStorage.setItem("redeemer_admin_secret", state.adminSecret);
    syncSecretField();
    showToast("管理密钥已记住");
  });

  $("#clear-secret").addEventListener("click", () => {
    state.adminSecret = "";
    sessionStorage.removeItem("redeemer_admin_secret");
    syncSecretField();
    showToast("管理密钥已清除");
  });

  $("#create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const result = $("#create-result");
    result.hidden = true;
    try {
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

  $("#codes-list").addEventListener("click", async (event) => {
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
