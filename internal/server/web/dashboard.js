// /dashboard：分组余额汇总、总消耗统计，超管还可创建与管理网关密钥。
(function () {
  "use strict";

  var LB = window.LB;
  var el = LB.el;
  var h = LB.h;

  var totals = {};
  var groups = [];
  var keys = [];
  var selected = {};
  var isSuper = false;

  function groupNameMap() {
    var map = {};
    groups.forEach(function (group) {
      map[group.id] = group.name;
    });
    return map;
  }

  // groupBalanceText：全部账号都没配置额度查询时显示无限余额。
  function groupBalanceText(group) {
    if (group.accounts > 0 && group.unlimited === group.accounts) {
      return "∞ 无限";
    }
    var text = LB.fmtMoney(group.balance, group.currency);
    if (group.unlimited) {
      text += " + " + group.unlimited + " 个无限";
    }
    return text;
  }

  function renderTotals() {
    var grid = el("totals-grid");
    LB.clear(grid);

    var balance = totals.balance || {};
    var token = totals.tokens || {};
    var request = totals.requests || {};
    var account = totals.accounts || {};
    var keyTotals = totals.keys || {};

    var unlimited = account.unlimited || 0;
    var balanceText = (account.total > 0 && unlimited === account.total)
      ? "∞ 无限"
      : LB.fmtMoney(balance.total, balance.currency);

    grid.appendChild(LB.stat(
      "账号总余额",
      balanceText,
      (account.enabled || 0) + " 个可用账号" + (unlimited ? " · " + unlimited + " 个无限额度" : "") +
      ((account.suspended || 0) ? " · " + account.suspended + " 个已暂停" : ""),
      "accent"
    ));
    grid.appendChild(LB.stat("累计消耗金额", LB.fmtMoney(balance.lifetime, balance.currency), "含已删除账号 " + LB.fmtMoney(balance.removedUsed, balance.currency)));
    grid.appendChild(LB.stat(
      "本站自算消耗金额",
      LB.fmtMoney(balance.localCost, balance.currency),
      "按本站 tokenizer + 账号单价结算 · 网关密钥侧 " + LB.fmtMoney(balance.keyCost, balance.currency)
    ));
    grid.appendChild(LB.stat(
      "消耗 tokens 总数",
      LB.fmtNumber(token.lifetime),
      (token.selfMetered ? "本站自算" : "上游口径") +
        " · 输入 " + LB.fmtNumber(token.prompt) + " / 输出 " + LB.fmtNumber(token.completion) +
        " · 上游自报 " + LB.fmtNumber(token.upstream)
    ));
    grid.appendChild(LB.stat("网关请求数", LB.fmtNumber(request.keys), "上游请求 " + LB.fmtNumber(request.accounts)));
    grid.appendChild(LB.stat("上游 API 数", LB.fmtNumber(account.apiCount), (account.total || 0) + " 个账号 / 已暂停 " + (account.suspended || 0) + " 个"));
    if (isSuper) {
      grid.appendChild(LB.stat("网关密钥", LB.fmtNumber(keyTotals.total), "已分配账号 " + LB.fmtNumber(keyTotals.assigned), (keyTotals.total && !keyTotals.assigned) ? "warn" : ""));
    }

    var meterNote = token.selfMetered ? " · tokens 与金额均由本站自算，上游自报值仅作对照" : "";
    el("totals-hint").textContent = ((account.suspended || 0) > 0
      ? "有 " + account.suspended + " 个账号已暂停（余额触及下限或频率异常），充值后在「分组与账号」里重新启用"
      : "分组 " + groups.length + " 个 · 负载均衡策略 " + (totals.strategy || "-")) + meterNote;
  }

  function renderGroups() {
    var list = el("group-list");
    LB.clear(list);
    if (!groups.length) {
      list.appendChild(h("div", { class: "empty", text: isSuper ? "还没有分组，先到「分组与账号」里创建" : "还没有分组" }));
      return;
    }
    groups.forEach(function (group) {
      var ratio = group.totalAmount > 0 ? group.balance / group.totalAmount : 0;
      list.appendChild(h("div", { class: "row" + (group.enabled ? "" : " muted") }, [
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [
            h("span", { text: group.name }),
            h("span", { class: "badge " + (group.enabled ? "good" : "bad"), text: group.enabled ? "已启用" : "已禁用" }),
            h("span", { class: "badge info", text: "余额 " + groupBalanceText(group) }),
            h("span", { class: "badge", text: group.usable + "/" + group.accounts + " 账号可用" })
          ]),
          h("div", {
            class: "row-sub",
            text: "消耗金额 " + LB.fmtMoney(group.lifetimeUsed, group.currency) +
              " · 自算消耗 " + LB.fmtMoney(group.localCost, group.currency) +
              " · 消耗 tokens " + LB.fmtNumber(group.lifetimeToken) +
              "（输入 " + LB.fmtNumber(group.promptTokens) + " / 输出 " + LB.fmtNumber(group.completionTokens) + "）" +
              " · 请求 " + LB.fmtNumber(group.requests) +
              " · " + group.apiCount + " 个上游 API · " + group.keys + " 个网关密钥" +
              (group.manualBalance ? " · " + group.manualBalance + " 个手动余额账号" : "")
          }),
          (group.totalAmount > 0) ? LB.progressBar(ratio, ratio < 0.15) : null
        ])
      ]));
    });
  }

  function keyBadges(key) {
    var badges = [];
    if (key.status === "active") {
      badges.push(h("span", { class: "badge good", text: "可用" }));
    } else if (key.status === "disabled") {
      badges.push(h("span", { class: "badge", text: "已禁用" }));
    } else if (key.status === "expired") {
      badges.push(h("span", { class: "badge bad", text: "已过期" }));
    } else {
      badges.push(h("span", { class: "badge warn", text: "额度用尽" }));
    }
    if (!key.accountId) {
      badges.push(h("span", { class: "badge warn", text: "待分配账号" }));
    }
    return badges;
  }

  function renderKeys() {
    var list = el("key-list");
    LB.clear(list);
    el("key-hint").textContent = keys.length ? keys.length + " 个密钥" : "";
    if (!keys.length) {
      list.appendChild(h("div", { class: "empty", text: "还没有网关密钥" }));
      return;
    }

    var names = groupNameMap();
    keys.forEach(function (key) {
      var box = h("input", { type: "checkbox", checked: !!selected[key.id] });
      box.addEventListener("change", function () {
        if (box.checked) {
          selected[key.id] = true;
        } else {
          delete selected[key.id];
        }
      });

      var detail = "分组 " + (key.groupId ? (names[key.groupId] || key.groupId) : "全部") +
        " · " + LB.fmtNumber(key.stats.requests) + " 次请求 · " + LB.fmtNumber(key.stats.totalTokens) + " tokens";
      if (key.quotaTokens) {
        detail += " / 上限 " + LB.fmtNumber(key.quotaTokens);
      }
      if (key.rateLimitPerMin) {
        detail += " · 限流 " + key.rateLimitPerMin + "/分钟";
      }
      if (key.expiresAt) {
        detail += " · 过期 " + LB.fmtTime(key.expiresAt);
      }
      if (key.allowedModels && key.allowedModels.length) {
        detail += " · 模型 " + key.allowedModels.join("/");
      }

      list.appendChild(h("div", { class: "row" }, [
        h("label", { class: "model-item", title: "选择" }, [box]),
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [
            h("span", { text: key.name }),
            h("span", { class: "mono", text: key.keyMasked })
          ].concat(keyBadges(key))),
          h("div", { class: "row-sub", text: detail })
        ]),
        h("div", { class: "row-actions" }, [
          h("button", { class: "btn btn-sm", text: "查看明文", onClick: function () { revealKey(key); } }),
          h("button", { class: "btn btn-quiet", text: "重置用量", onClick: function () { resetUsage(key); } }),
          h("button", { class: "btn btn-danger btn-sm", text: "删除", onClick: function () { deleteKey(key); } })
        ])
      ]));
    });
  }

  function renderGroupOptions() {
    var select = el("key-group");
    var previous = select.value;
    LB.clear(select);
    select.appendChild(h("option", { value: "", text: "不限定（全部分组）" }));
    groups.forEach(function (group) {
      select.appendChild(h("option", {
        value: group.id,
        text: group.name + (group.enabled ? "" : "（已禁用）")
      }));
    });
    if (previous) {
      select.value = previous;
    }
  }

  function renderUsage() {
    var base = window.location.origin + "/v1";
    el("usage-base").textContent = base;
    el("usage-models").textContent = [
      "GET  " + base + "/models          # 列出全部可用模型",
      "GET  " + base + "/models/{model}  # 查询单个模型",
      "",
      "响应严格遵循 OpenAI 规范：",
      '{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","created":1700000000,"owned_by":"laskah"}]}',
      "",
      "不带 Authorization 头时返回 200 与空列表（附 hint 字段），带无效密钥仍返回 401/403。"
    ].join("\n");
    el("usage-curl").textContent = [
      "curl " + base + "/chat/completions \\",
      '  -H "Authorization: Bearer <网关密钥>" \\',
      '  -H "Content-Type: application/json" \\',
      '  -d \'{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}\''
    ].join("\n");
    el("usage-responses").textContent = [
      "curl " + base + "/responses \\",
      '  -H "Authorization: Bearer <网关密钥>" \\',
      '  -H "Content-Type: application/json" \\',
      '  -d \'{"model":"gpt-4o-mini","input":"你好"}\'',
      "",
      "同时支持 stream:true；流式与非流式都会被重写为本站自算 usage。"
    ].join("\n");
    el("usage-metering").textContent = [
      "tokens 由本站 tokenizer 统计，不采用上游返回的 usage（部分站点会谎报）。",
      "上游自报值仍会保留在统计里，字段 upstreamTokens，仅用于对照。",
      "账号可选计价方式：",
      "  none        不计价，仅统计 tokens",
      "  per_mtoken  按每 1M tokens 单价结算（输入+输出合并计量）",
      "  per_call    按每次请求固定单价结算",
      "开启手动余额后，余额由初始额度减去本站自算消耗得出，无需上游额度查询接口。",
      "未配置额度查询且未开启手动余额的账号显示为「∞ 无限」。"
    ].join("\n");
  }

  function showReveal(lines) {
    var box = el("key-reveal");
    LB.clear(box);
    box.hidden = false;
    lines.forEach(function (line) {
      box.appendChild(h("div", { text: line }));
    });
    box.appendChild(h("button", {
      class: "btn btn-quiet",
      text: "复制全部",
      onClick: function () {
        LB.copyText(lines.join("\n"));
      }
    }));
  }

  function keyPayload() {
    var models = el("key-models").value.trim();
    var expires = el("key-expires").value.trim();
    return {
      name: el("key-name").value.trim() || "gateway-key",
      prefix: el("key-prefix").value.trim(),
      groupId: el("key-group").value,
      allowedModels: models,
      quotaTokens: el("key-quota").value.trim(),
      rateLimitPerMin: el("key-rate").value.trim(),
      expiresAt: expires ? new Date(expires).toISOString() : ""
    };
  }

  async function createKey(button) {
    button.disabled = true;
    try {
      var response = await LB.request("POST", "/admin/keys", keyPayload());
      var created = response.data || {};
      showReveal([created.name + "  " + created.key]);
      LB.toast("密钥已创建，请立即保存明文", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    } finally {
      button.disabled = false;
    }
  }

  async function bulkCreate(button) {
    var count = parseInt(el("key-count").value, 10);
    if (!(count >= 1 && count <= 500)) {
      LB.toast("批量数量需要是 1-500 的整数", "error");
      return;
    }
    button.disabled = true;
    try {
      var response = await LB.request("POST", "/admin/keys/bulk", { count: count, template: keyPayload() });
      var list = response.data || [];
      showReveal(list.map(function (item) {
        return item.name + "  " + item.key;
      }));
      LB.toast("已批量创建 " + list.length + " 个密钥", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    } finally {
      button.disabled = false;
    }
  }

  async function revealKey(key) {
    try {
      var response = await LB.request("GET", "/admin/keys/" + encodeURIComponent(key.id) + "/reveal");
      showReveal([response.data.name + "  " + response.data.key]);
      LB.copyText(response.data.key);
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function resetUsage(key) {
    try {
      await LB.request("POST", "/admin/keys/" + encodeURIComponent(key.id) + "/reset-usage");
      LB.toast("用量已重置", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function deleteKey(key) {
    if (!window.confirm("删除密钥「" + key.name + "」？删除后使用它的客户端会立即失效。")) {
      return;
    }
    try {
      await LB.request("DELETE", "/admin/keys/" + encodeURIComponent(key.id));
      delete selected[key.id];
      LB.toast("密钥已删除", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function batchDelete() {
    var ids = Object.keys(selected);
    if (!ids.length) {
      LB.toast("请先勾选要删除的密钥", "error");
      return;
    }
    if (!window.confirm("删除所选 " + ids.length + " 个密钥？")) {
      return;
    }
    try {
      var response = await LB.request("DELETE", "/admin/keys/batch", { ids: ids });
      selected = {};
      LB.toast("已删除 " + (response.removed || 0) + " 个密钥", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  function exportCsv() {
    if (!keys.length) {
      LB.toast("暂无密钥可导出", "error");
      return;
    }
    var names = groupNameMap();
    var rows = [["name", "keyMasked", "group", "status", "requests", "tokens", "createdAt"]];
    keys.forEach(function (key) {
      rows.push([
        key.name,
        key.keyMasked,
        key.groupId ? (names[key.groupId] || key.groupId) : "",
        key.status,
        String(key.stats.requests),
        String(key.stats.totalTokens),
        key.createdAt
      ]);
    });
    var csv = rows.map(function (row) {
      return row.map(function (cell) {
        return '"' + String(cell == null ? "" : cell).replace(/"/g, '""') + '"';
      }).join(",");
    }).join("\r\n");

    var blob = new Blob(["\ufeff" + csv], { type: "text/csv;charset=utf-8" });
    var url = URL.createObjectURL(blob);
    var link = document.createElement("a");
    link.href = url;
    link.download = "laskah-keys.csv";
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    URL.revokeObjectURL(url);
  }

  function resetForm() {
    ["key-name", "key-prefix", "key-quota", "key-rate", "key-expires", "key-models"].forEach(function (id) {
      el(id).value = "";
    });
    el("key-count").value = "10";
    el("key-group").value = "";
    el("key-reveal").hidden = true;
  }

  // openPasswordModal 让任意角色改自己的登录口令。
  function openPasswordModal() {
    var current = LB.h("input", { type: "password", autocomplete: "current-password" });
    var next = LB.h("input", { type: "password", autocomplete: "new-password", placeholder: "至少 8 个字符" });
    var confirm = LB.h("input", { type: "password", autocomplete: "new-password" });

    function field(label, node, hint) {
      return h("div", { class: "field full" }, [
        h("label", { text: label }),
        node,
        hint ? h("span", { class: "hint-text", text: hint }) : null
      ]);
    }

    LB.modal({
      title: "修改我的密码",
      subtitle: "修改成功后当前账户需要重新登录",
      body: h("div", { class: "form-grid" }, [
        field("当前密码", current),
        field("新密码", next, "至少 8 个字符；首尾空格与换行会被忽略"),
        field("确认新密码", confirm)
      ]),
      confirmText: "更新密码",
      onConfirm: async function () {
        // 与服务端一致地忽略首尾空白，避免粘贴带尾随空格后改完密码登不进去。
        var nextValue = next.value.trim();
        if (nextValue.length < 8) {
          LB.toast("新密码至少 8 个字符", "error");
          return false;
        }
        if (nextValue !== confirm.value.trim()) {
          LB.toast("两次输入的密码不一致", "error");
          return false;
        }
        await LB.request("POST", "/admin/password", { current: current.value, next: nextValue });
        LB.toast("密码已更新，即将重新登录", "ok");
        window.setTimeout(function () {
          window.location.href = "/login";
        }, 1200);
      }
    });
  }

  async function refreshBalances(button) {
    button.disabled = true;
    button.textContent = "刷新中…";
    try {
      var response = await LB.request("POST", "/admin/accounts/refresh-all");
      var results = response.data || [];
      var suspended = results.filter(function (item) { return item.suspended; }).length;
      var failed = results.filter(function (item) { return item.error; }).length;
      LB.toast("已刷新 " + results.length + " 个账号，自动暂停 " + suspended + " 个，失败 " + failed + " 个", failed ? "error" : "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    } finally {
      button.disabled = false;
      button.textContent = "手动刷新全部余额";
    }
  }

  // queryTotalBalance：先强制刷新全部账号，再把「总余额」按来源拆开显示。
  // 只给一个总数容易误判——查询失败的账号余额是旧值，必须一起说清楚。
  async function queryTotalBalance(button) {
    var original = button.textContent;
    button.disabled = true;
    button.textContent = "查询中…";
    try {
      var response = await LB.request("POST", "/admin/accounts/balance-query");
      var data = response.data || {};
      var t = data.totals || {};
      var balance = t.balance || {};
      var account = t.accounts || {};
      var token = t.tokens || {};
      var box = el("balance-report");
      var lines = [
        "总余额（可计金额账号合计）  " + LB.fmtMoney(balance.total, balance.currency),
        "  ├ 上游查询得到          " + LB.fmtMoney(balance.queriedBalance, balance.currency) + "   " + (account.queried || 0) + " 个账号",
        "  └ 手动余额本地扣减      " + LB.fmtMoney(balance.manualAmount, balance.currency) + "   " + (account.manualBalance || 0) + " 个账号",
        "无限额度账号              " + (account.unlimited || 0) + " 个（不计入总额）",
        "累计消耗金额              " + LB.fmtMoney(balance.lifetime, balance.currency) + "（含已删除账号 " + LB.fmtMoney(balance.removedUsed, balance.currency) + "）",
        "本站自算消耗金额          " + LB.fmtMoney(balance.localCost, balance.currency),
        "消耗 tokens（本站自算）   " + LB.fmtNumber(token.lifetime) + "（上游自报 " + LB.fmtNumber(token.upstream) + "）",
        "",
        "本次查询 " + (data.queried || 0) + " 个账号 · 失败 " + (data.failed || 0) + " 个 · 暂停 " + (data.suspended || 0) + " 个 · 删除 " + (data.deleted || 0) + " 个",
        (data.failed ? "失败账号沿用上次余额，总额可能偏高，请在「分组与账号」里单独复查。" : "全部账号查询成功，总额为最新值。"),
        "统计时间 " + LB.fmtTime(new Date().toISOString())
      ];
      box.hidden = false;
      box.textContent = lines.join("\n");
      totals = t;
      totals.strategy = response.strategy || totals.strategy;
      groups = data.groups || totals.groups || [];
      renderTotals();
      renderGroups();
      LB.toast("总余额 " + LB.fmtMoney(balance.total, balance.currency) + (data.failed ? "（" + data.failed + " 个账号查询失败）" : ""), data.failed ? "warn" : "ok");
    } catch (err) {
      LB.toast(err.message, "error");
    } finally {
      button.disabled = false;
      button.textContent = original;
    }
  }

  async function loadAll() {
    try {
      var response = await LB.request("GET", "/admin/dashboard");
      totals = response.data || {};
      totals.strategy = response.strategy;
      groups = totals.groups || [];
      keys = response.keys || [];
      Object.keys(selected).forEach(function (id) {
        if (!keys.some(function (key) { return key.id === id; })) {
          delete selected[id];
        }
      });
      renderTotals();
      renderGroups();
      if (isSuper) {
        renderGroupOptions();
        renderKeys();
      }
      var accountTotals = totals.accounts || {};
      el("refresh-hint").textContent = "共 " + (accountTotals.total || 0) + " 个账号（上游查询 " +
        (accountTotals.queried || 0) + " · 手动余额 " + (accountTotals.manualBalance || 0) + " · 无限 " +
        (accountTotals.unlimited || 0) + "）· 刷新后余额触及各自下限的账号会被自动暂停";
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function init() {
    var session = await LB.loadSession();
    if (!session) {
      return;
    }
    isSuper = !!session.isSuper;
    LB.nav("dashboard");
    renderUsage();

    // 密钥管理与余额刷新都属于超管能力，普通管理员只读看板。
    el("keys-area").hidden = !isSuper;
    el("refresh-balances").hidden = !isSuper;
    el("query-total").hidden = !isSuper;
    if (isSuper) {
      el("query-total").addEventListener("click", function (event) {
        queryTotalBalance(event.currentTarget);
      });
    }

    if (isSuper) {
      el("key-create").addEventListener("click", function (event) {
        createKey(event.currentTarget);
      });
      el("key-bulk").addEventListener("click", function (event) {
        bulkCreate(event.currentTarget);
      });
      el("key-reset").addEventListener("click", resetForm);
      el("key-select-all").addEventListener("click", function () {
        keys.forEach(function (key) {
          selected[key.id] = true;
        });
        renderKeys();
      });
      el("key-select-none").addEventListener("click", function () {
        selected = {};
        renderKeys();
      });
      el("key-export").addEventListener("click", exportCsv);
      el("key-batch-delete").addEventListener("click", batchDelete);
      el("refresh-balances").addEventListener("click", function (event) {
        refreshBalances(event.currentTarget);
      });
    }

    el("reload-dashboard").addEventListener("click", function (event) {
      var button = event.currentTarget;
      button.disabled = true;
      loadAll().finally(function () {
        button.disabled = false;
      });
    });
    el("copy-base").addEventListener("click", function () {
      LB.copyText(window.location.origin + "/v1");
    });
    el("change-password").addEventListener("click", openPasswordModal);

    await loadAll();
  }

  init();
})();
