// /manage：分组（含启停）、账号（弹窗创建、启停）、管理员账户、账号移除记录。
(function () {
  "use strict";

  var LB = window.LB;
  var el = LB.el;
  var h = LB.h;

  var MAX_KEYS = 5;
  var groups = [];
  var accounts = [];
  var users = [];
  var removed = [];

  // ---------- 工具 ----------

  // 先按行剔除注释，再在行内按空白/逗号/分号切分，与后端 parseKeyLines 保持一致。
  function parseKeys(text) {
    var seen = {};
    var result = [];
    String(text || "").split(/\r?\n/).forEach(function (line) {
      var row = line.trim();
      if (!row || row.indexOf("#") === 0 || row.indexOf("//") === 0) {
        return;
      }
      row.split(/[\s,;]+/).forEach(function (raw) {
        var trimmed = raw.trim().replace(/^["',;]+|["',;]+$/g, "");
        if (!trimmed || seen[trimmed]) {
          return;
        }
        seen[trimmed] = true;
        result.push(trimmed);
      });
    });
    return result;
  }

  function groupNameMap() {
    var map = {};
    groups.forEach(function (group) {
      map[group.id] = group.name;
    });
    return map;
  }

  function field(labelText, node, hint, required) {
    return h("div", { class: "field full" }, [
      h("label", {}, [h("span", { text: labelText }), required ? h("span", { class: "req", text: "*" }) : null]),
      node,
      hint ? h("span", { class: "hint-text", text: hint }) : null
    ]);
  }

  // ---------- 分组 ----------

  function renderGroups() {
    var list = el("group-list");
    LB.clear(list);
    el("group-hint").textContent = groups.length
      ? groups.length + " 个分组 · " + groups.filter(function (g) { return g.enabled; }).length + " 个已启用"
      : "分组用于隔离账号池，网关密钥可绑定到指定分组";

    if (!groups.length) {
      list.appendChild(h("div", { class: "empty", text: "还没有分组，点击「创建分组」开始" }));
      return;
    }

    groups.forEach(function (group) {
      var badges = [
        h("span", { class: "badge " + (group.enabled ? "good" : "bad"), text: group.enabled ? "已启用" : "已禁用" }),
        h("span", { class: "badge info", text: "余额 " + groupBalanceText(group) }),
        h("span", { class: "badge", text: group.accounts + " 账号 / " + group.apiCount + " API" })
      ];
      list.appendChild(h("div", { class: "row" + (group.enabled ? "" : " muted") }, [
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [h("span", { text: group.name })].concat(badges)),
          h("div", {
            class: "row-sub",
            text: "消耗 " + LB.fmtMoney(group.lifetimeUsed) + " · " + LB.fmtNumber(group.lifetimeToken) +
              " tokens · " + group.keys + " 个网关密钥 · " + group.usable + " 个账号可承接流量" +
              (group.unlimited ? " · " + group.unlimited + " 个无限额度" : "") +
              (group.note ? " · " + group.note : "")
          })
        ]),
        h("div", { class: "row-actions" }, [
          LB.toggle(group.enabled, function (checked, input) {
            toggleGroup(group, checked, input);
          }, "启用分组"),
          h("button", {
            class: "btn btn-sm",
            text: "刷新余额",
            onClick: function (event) {
              refreshGroup(group, event.currentTarget);
            }
          }),
          h("button", {
            class: "btn btn-danger btn-sm",
            text: "删除",
            onClick: function () {
              deleteGroup(group);
            }
          })
        ])
      ]));
    });
  }

  // groupBalanceText 全部账号都没配置额度查询时显示无限。
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

  function openGroupModal() {
    var name = h("input", { type: "text", placeholder: "例如：团队 A", maxlength: 64 });
    var note = h("input", { type: "text", placeholder: "可选" });
    var enabled = h("input", { type: "checkbox", checked: true, id: "modal-group-enabled" });

    LB.modal({
      title: "创建用户分组",
      subtitle: "分组内可添加多个账号，账号之间自动负载均衡",
      body: h("div", { class: "form-grid" }, [
        field("分组名称", name, "键入一个便于识别的名称", true),
        field("备注", note),
        h("div", { class: "field full" }, [
          h("div", { class: "switch-row" }, [
            h("div", {}, [
              h("label", { text: "创建后立即启用" }),
              h("div", { class: "hint-text", text: "禁用的分组不会承接任何流量，但保留账号与余额数据" })
            ]),
            h("span", { class: "switch" }, [enabled, h("span", { class: "switch-track" })])
          ])
        ])
      ]),
      confirmText: "创建分组",
      onConfirm: async function () {
        var value = name.value.trim();
        if (!value) {
          LB.toast("请输入分组名称", "error");
          return false;
        }
        await LB.request("POST", "/admin/groups", {
          name: value,
          note: note.value.trim(),
          enabled: enabled.checked
        });
        LB.toast("分组已创建", "ok");
        await loadAll();
      }
    });
  }

  async function toggleGroup(group, enabled, input) {
    input.disabled = true;
    try {
      await LB.request("POST", "/admin/groups/" + encodeURIComponent(group.id) + "/enable", { enabled: enabled });
      LB.toast("分组「" + group.name + "」已" + (enabled ? "启用" : "禁用"), "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
      input.checked = !enabled;
    } finally {
      input.disabled = false;
    }
  }

  async function refreshGroup(group, button) {
    var original = button.textContent;
    button.disabled = true;
    button.textContent = "刷新中…";
    try {
      var response = await LB.request("POST", "/admin/groups/" + encodeURIComponent(group.id) + "/refresh");
      var results = response.data || [];
      var suspended = results.filter(function (item) { return item.suspended; }).length;
      var failed = results.filter(function (item) { return item.error; }).length;
      LB.toast("分组「" + group.name + "」已刷新 " + results.length + " 个账号，自动暂停 " + suspended + " 个，失败 " + failed + " 个", failed ? "error" : "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
      button.disabled = false;
      button.textContent = original;
    }
  }

  async function deleteGroup(group) {
    if (!window.confirm("删除分组「" + group.name + "」会同时删除其名下 " + group.accounts + " 个账号与 " + group.apiCount + " 个 API，确定继续？")) {
      return;
    }
    try {
      await LB.request("DELETE", "/admin/groups/" + encodeURIComponent(group.id));
      LB.toast("分组已删除", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  // ---------- 账号 ----------

  function accountBadges(account) {
    var badges = [];
    if (account.suspended) {
      badges.push(h("span", { class: "badge bad", text: account.exhausted ? "余额不足已暂停" : "已暂停" }));
    } else if (!account.enabled) {
      badges.push(h("span", { class: "badge", text: "已禁用" }));
    } else if (account.exhausted) {
      badges.push(h("span", { class: "badge bad", text: "余额耗尽" }));
    } else if (account.checkError) {
      badges.push(h("span", { class: "badge warn", text: "查询失败" }));
    } else {
      badges.push(h("span", { class: "badge good", text: "可用" }));
    }
    if (account.rateLimitPerMin) {
      badges.push(h("span", { class: "badge", text: account.rateLimitPerMin + " 次/分钟" }));
    }
    if (account.unlimited) {
      badges.push(h("span", { class: "badge info", text: "∞ 无限余额" }));
    } else {
      badges.push(h("span", { class: "badge info", text: "余额 " + LB.fmtMoney(account.balance, account.currency) }));
      // 余额已进入「下一次刷新就会暂停」的区间时提前提示，避免管理员措手不及。
      var floor = account.balanceFloor || 0;
      if (!account.exhausted && floor > 0 && account.balance <= floor * 2) {
        badges.push(h("span", { class: "badge warn", text: "接近下限 " + LB.fmtMoney(floor, account.currency) }));
      }
    }
    if (account.planName) {
      badges.push(h("span", { class: "badge", text: account.planName }));
    }
    badges.push(h("span", { class: "badge", text: account.apiCount + "/" + account.maxApiCount + " API" }));
    return badges;
  }

  function renderAccounts() {
    var list = el("account-list");
    LB.clear(list);
    if (!accounts.length) {
      list.appendChild(h("div", { class: "empty", text: "还没有账号，点击「创建账号」并在弹窗中填写 API 配置" }));
      return;
    }

    var names = groupNameMap();
    accounts.forEach(function (account) {
      var detail = "分组 " + (names[account.groupId] || "未分组");
      if (account.unlimited) {
        detail += " · 未配置额度查询，视为无限余额";
      } else {
        detail += " · 已用 " + LB.fmtMoney(account.usedAmount, account.currency) +
          " · 余额下限 " + LB.fmtMoney(account.balanceFloor || 0, account.currency) +
          " · 查询于 " + LB.fmtTime(account.checkedAt);
      }
      detail += " · " + LB.fmtNumber(account.stats.totalTokens) + " tokens · " +
        LB.fmtNumber(account.stats.requests) + " 次请求";
      if (account.checkError) {
        detail += " · " + account.checkError;
      }
      if (account.suspended && account.suspendReason) {
        detail += " · " + account.suspendReason;
      }

      var ratio = account.totalAmount > 0 ? account.balance / account.totalAmount : 0;
      list.appendChild(h("div", { class: "row" }, [
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [h("span", { text: account.name })].concat(accountBadges(account))),
          h("div", { class: "row-sub", text: detail }),
          (!account.unlimited && account.totalAmount > 0) ? LB.progressBar(ratio, ratio < 0.15) : null
        ]),
        h("div", { class: "row-actions" }, [
          h("button", {
            class: "btn btn-sm" + (account.hasBalanceQuery ? " btn-primary" : ""),
            text: account.hasBalanceQuery ? "手动刷新" : "查看余额",
            disabled: !account.hasBalanceQuery,
            title: account.hasBalanceQuery ? "立即查询该账号余额" : "未配置额度查询，余额视为无限",
            onClick: function (event) {
              refreshAccount(account, event.currentTarget);
            }
          }),
          LB.toggle(!account.suspended && account.enabled, function (checked, input) {
            toggleAccount(account, checked, input);
          }, "启用账号"),
          h("button", {
            class: "btn btn-danger btn-sm",
            text: "删除",
            onClick: function () {
              deleteAccount(account);
            }
          })
        ])
      ]));
    });
  }

  // toggleAccount 启用或暂停账号。
  //
  // 启用会在服务端顺带刷一次余额：充值后重新启用应当立刻看到真实余额，
  // 否则账号会带着旧的耗尽数据回到池子里，第一批请求仍会被上游拒绝。
  async function toggleAccount(account, enabled, input) {
    input.disabled = true;
    try {
      var response = await LB.request("POST", "/admin/accounts/" + encodeURIComponent(account.id) + "/enable", { enabled: enabled });
      var refreshed = response.refresh || {};
      if (!enabled) {
        LB.toast("账号「" + account.name + "」已暂停，不再参与分配", "ok");
      } else if (refreshed.suspended) {
        LB.toast("账号「" + account.name + "」余额仍低于下限，已再次暂停", "error");
      } else if (refreshed.error) {
        LB.toast("账号已启用，但余额查询失败：" + refreshed.error, "error");
      } else {
        LB.toast("账号「" + account.name + "」已启用", "ok");
      }
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
      input.checked = !enabled;
      input.disabled = false;
    }
  }

  // openAccountModal 是「先创建账号，再在居中弹窗里填 API 配置」的入口。
  function openAccountModal() {
    if (!groups.length) {
      LB.toast("请先创建一个用户分组", "error");
      return;
    }

    var probed = [];
    var selected = {};

    var groupSelect = h("select", {});
    groups.forEach(function (group) {
      groupSelect.appendChild(h("option", {
        value: group.id,
        text: group.name + (group.enabled ? "" : "（已禁用）")
      }));
    });

    var nameInput = h("input", { type: "text", placeholder: "例如：newapi-01" });
    var keysInput = h("textarea", {
      rows: 6,
      spellcheck: false,
      placeholder: "每行一个 API Key，最多 " + MAX_KEYS + " 个"
    });
    var keysCount = h("span", { class: "hint-text", text: "已识别 0 个 Key（上限 " + MAX_KEYS + "）" });
    keysInput.addEventListener("input", function () {
      var keys = parseKeys(keysInput.value);
      keysCount.textContent = "已识别 " + keys.length + " 个 Key（上限 " + MAX_KEYS + "）" +
        (keys.length > MAX_KEYS ? " · 超出部分将被忽略" : "");
    });

    var baseInput = h("input", { type: "text", placeholder: "https://api.newapi.com/v1", spellcheck: false });
    var modelGrid = h("div", { class: "model-grid" });
    var modelStatus = h("span", { class: "hint-text", text: "尚未获取，留空表示接受全部模型" });

    function renderModels() {
      LB.clear(modelGrid);
      if (!probed.length) {
        modelGrid.appendChild(h("div", { class: "empty", text: "点击「获取模型列表」后在此勾选" }));
        modelStatus.textContent = "尚未获取，留空表示接受全部模型";
        return;
      }
      probed.forEach(function (model) {
        var box = h("input", { type: "checkbox", checked: !!selected[model] });
        box.addEventListener("change", function () {
          if (box.checked) {
            selected[model] = true;
          } else {
            delete selected[model];
          }
          updateModelStatus();
        });
        modelGrid.appendChild(h("label", { class: "model-item", title: model }, [box, h("span", { text: model })]));
      });
      updateModelStatus();
    }

    function updateModelStatus() {
      var count = Object.keys(selected).length;
      modelStatus.textContent = "共 " + probed.length + " 个模型，已勾选 " + count + " 个" + (count ? "" : "（留空表示接受全部）");
    }

    // 探测失败的原因往往很长（上游可能整页返回 HTML），固定展示在弹窗里而不是只弹提示条。
    var probeError = h("div", { class: "inline-error" });
    probeError.style.display = "none";

    function showProbeError(text) {
      probeError.textContent = text;
      probeError.style.display = text ? "" : "none";
    }

    var probeButton = h("button", { class: "btn btn-sm", type: "button", text: "获取模型列表" });
    probeButton.addEventListener("click", async function () {
      var keys = parseKeys(keysInput.value);
      if (!baseInput.value.trim()) {
        LB.toast("请先填写 Base URL", "error");
        return;
      }
      if (!keys.length) {
        LB.toast("请先粘贴至少一个 API Key", "error");
        return;
      }
      probeButton.disabled = true;
      probeButton.textContent = "获取中…";
      showProbeError("");
      try {
        var response = await LB.request("POST", "/admin/models/probe", {
          baseUrl: baseInput.value.trim(),
          apiKey: keys[0]
        });
        probed = response.data || [];
        selected = {};
        renderModels();
        if (!probed.length) {
          showProbeError("上游返回了空的模型列表，留空勾选表示接受全部模型。");
        }
        LB.toast("获取到 " + probed.length + " 个模型（" + response.latencyMs + " ms）", "ok");
      } catch (err) {
        showProbeError(err.message);
        LB.toast("获取模型列表失败，详情见下方说明", "error");
      } finally {
        probeButton.disabled = false;
        probeButton.textContent = "获取模型列表";
      }
    });

    var selectAll = h("button", { class: "btn btn-quiet", type: "button", text: "全选" });
    selectAll.addEventListener("click", function () {
      probed.forEach(function (model) { selected[model] = true; });
      renderModels();
    });
    var clearAll = h("button", { class: "btn btn-quiet", type: "button", text: "清空" });
    clearAll.addEventListener("click", function () {
      selected = {};
      renderModels();
    });

    var siteInput = h("input", { type: "text", placeholder: "https://api.newapi.com", spellcheck: false });
    var tokenInput = h("input", { type: "password", placeholder: "在『安全设置』里生成", autocomplete: "off", spellcheck: false });
    var userIdInput = h("input", { type: "text", placeholder: "例如：114514", spellcheck: false });
    var timeoutInput = h("input", { type: "number", min: 1, max: 120, value: "10" });
    var intervalInput = h("input", { type: "number", min: 0, max: 1440, value: "0" });
    var minBalanceInput = h("input", { type: "number", min: 0, step: "0.0001", value: "0" });
    var reqRefreshInput = h("input", { type: "number", min: 1, max: 3600, value: "60" });
    var autoSuspend = h("input", { type: "checkbox", checked: true });
    var refreshOnRequest = h("input", { type: "checkbox", checked: true });

    // 频率限制：默认不限制；勾选后才要求填写「一分钟能请求多少次」。
    var rateLimited = h("input", { type: "checkbox" });
    var rateLimitInput = h("input", { type: "number", min: 1, max: 100000, placeholder: "例如：60", disabled: true });
    var rateLimitField = field("每分钟请求次数", rateLimitInput, "达到该次数后本账号本分钟不再参与分配，网关自动切到其它账号");
    rateLimitField.style.display = "none";
    rateLimited.addEventListener("change", function () {
      rateLimitInput.disabled = !rateLimited.checked;
      rateLimitField.style.display = rateLimited.checked ? "" : "none";
      if (rateLimited.checked && !rateLimitInput.value) {
        rateLimitInput.value = "60";
      }
    });

    function switchBlock(labelText, input, note) {
      return h("div", { class: "field full" }, [
        h("div", { class: "switch-row" }, [
          h("div", {}, [h("label", { text: labelText }), note ? h("div", { class: "hint-text", text: note }) : null]),
          h("span", { class: "switch" }, [input, h("span", { class: "switch-track" })])
        ])
      ]);
    }

    var body = h("div", {}, [
      h("div", { class: "form-grid" }, [
        field("所属分组", groupSelect, null, true),
        field("用户名称", nameInput, "仅用于界面识别", true),
        h("div", { class: "field full" }, [
          h("label", {}, [h("span", { text: "API Key 批量粘贴" }), h("span", { class: "req", text: "*" })]),
          keysInput,
          keysCount
        ]),
        field("Base URL", baseInput, "请求时自动拼接 /chat/completions，走 OpenAI chat completions 兼容协议", true),
        h("div", { class: "field full" }, [
          h("label", { text: "模型列表" }),
          h("div", { class: "btn-row" }, [probeButton, selectAll, clearAll, modelStatus]),
          probeError,
          modelGrid
        ])
      ]),
      h("div", { class: "section-head" }, [
        h("h2", { text: "凭证配置" }),
        h("span", { class: "hint", text: "留空则自动使用供应商配置；留空即视为无限余额" })
      ]),
      h("div", { class: "form-grid" }, [
        field("请求地址", siteInput, "New API 站点地址，留空则用 Base URL 去掉 /v1"),
        field("访问令牌（在个人安全设置里获取）", tokenInput, "站点个人设置 → 安全设置 → 生成访问令牌"),
        field("用户 ID", userIdInput, "New API 个人设置页可见"),
        field("超时时间（秒）", timeoutInput),
        field("自动查询间隔（分钟，0 表示不自动查询）", intervalInput),
        field("最低余额（低于则视为耗尽）", minBalanceInput, "内置安全线 $0.50：填 0 也会按 $0.50 执行，只有填更大的值才会生效"),
        field("请求时刷新间隔（秒）", reqRefreshInput, "调用到达时若余额数据超过该时长未更新，先查一次再分配流量"),
        switchBlock("余额耗尽时自动暂停账号", autoSuspend, "暂停不删除数据，充值后在列表里重新启用即可恢复；查询失败不会触发暂停"),
        switchBlock("请求时刷新余额", refreshOnRequest, "调用到达时先确认余额，防止余额用完仍继续使用该账号")
      ]),
      h("div", { class: "section-head" }, [
        h("h2", { text: "频率限制" }),
        h("span", { class: "hint", text: "留空则不限制" })
      ]),
      h("div", { class: "form-grid" }, [
        switchBlock("该账号有频率限制", rateLimited, "不开启表示无限制；开启后需填写每分钟允许的请求次数"),
        rateLimitField
      ])
    ]);

    renderModels();

    LB.modal({
      title: "创建账号",
      subtitle: "保存后只能查询余额、启停或删除账号，配置不可修改也不会回显",
      wide: true,
      body: body,
      confirmText: "确定保存配置",
      onConfirm: async function () {
        var keys = parseKeys(keysInput.value);
        if (!nameInput.value.trim()) {
          LB.toast("请输入用户名称", "error");
          return false;
        }
        if (!baseInput.value.trim()) {
          LB.toast("请填写 Base URL", "error");
          return false;
        }
        if (!keys.length) {
          LB.toast("请粘贴至少一个 API Key", "error");
          return false;
        }
        if (rateLimited.checked && !(Number(rateLimitInput.value) >= 1)) {
          LB.toast("请填写每分钟请求次数，或关闭频率限制", "error");
          return false;
        }

        var response = await LB.request("POST", "/admin/accounts", {
          groupId: groupSelect.value,
          name: nameInput.value.trim(),
          baseUrl: baseInput.value.trim(),
          siteUrl: siteInput.value.trim(),
          accessToken: tokenInput.value,
          userId: userIdInput.value.trim(),
          timeoutSeconds: timeoutInput.value,
          queryIntervalMin: intervalInput.value,
          minBalance: minBalanceInput.value,
          requestRefreshSec: reqRefreshInput.value,
          rateLimitPerMin: rateLimited.checked ? rateLimitInput.value : "",
          autoSuspend: autoSuspend.checked,
          refreshOnRequest: refreshOnRequest.checked,
          keyList: keys,
          selectedModels: Object.keys(selected)
        });

        var message = "账号已保存，导入 " + (response.created || 0) + " 个 API";
        if (response.skipped && response.skipped.length) {
          message += "，忽略 " + response.skipped.length + " 条（超出 " + MAX_KEYS + " 上限）";
        }
        if (response.data && response.data.checkError) {
          LB.toast(message + "；余额查询失败：" + response.data.checkError, "error");
        } else if (response.data && response.data.unlimited) {
          LB.toast(message + "，未配置额度查询，按无限余额处理", "ok");
        } else {
          LB.toast(message + "，当前余额 " + LB.fmtMoney(response.data ? response.data.balance : 0), "ok");
        }
        await loadAll();
      }
    });
  }

  async function refreshAccount(account, button) {
    var original = button.textContent;
    button.disabled = true;
    button.textContent = "查询中…";
    try {
      var response = await LB.request("POST", "/admin/accounts/" + encodeURIComponent(account.id) + "/refresh");
      var result = response.data || {};
      if (result.suspended) {
        LB.toast("余额已触及下限（$0.50 安全线），账号「" + account.name + "」已自动暂停，充值后重新启用", "error");
      } else if (result.error) {
        LB.toast("查询失败：" + result.error, "error");
      } else {
        LB.toast(account.name + " 余额 " + LB.fmtMoney(result.balance) + "，已用 " + LB.fmtMoney(result.usedAmount), "ok");
      }
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
      button.disabled = false;
      button.textContent = original;
    }
  }

  async function deleteAccount(account) {
    if (!window.confirm("删除账号「" + account.name + "」及其 " + account.apiCount + " 个 API？")) {
      return;
    }
    try {
      await LB.request("DELETE", "/admin/accounts/" + encodeURIComponent(account.id));
      LB.toast("账号已删除", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function refreshAll(button) {
    var original = button.textContent;
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
      button.textContent = original;
    }
  }

  // ---------- 管理员账户 ----------

  function renderUsers() {
    var list = el("user-list");
    LB.clear(list);
    el("users-hint").textContent = users.length
      ? users.length + " 个账户 · 超管可访问全部页面，管理员仅能查看数据看板"
      : "超级管理员可访问全部页面；管理员只能查看数据看板";

    if (!users.length) {
      list.appendChild(h("div", { class: "empty", text: "暂无账户" }));
      return;
    }

    var me = LB.session();
    users.forEach(function (user) {
      var badges = [
        h("span", { class: "badge " + (user.isSuper ? "info" : ""), text: user.isSuper ? "超级管理员" : "管理员" }),
        h("span", { class: "badge " + (user.enabled ? "good" : "bad"), text: user.enabled ? "已启用" : "已禁用" })
      ];
      if (me && me.user === user.username) {
        badges.push(h("span", { class: "badge warn", text: "当前登录" }));
      }

      var actions = [
        h("button", {
          class: "btn btn-sm",
          text: "重置口令",
          onClick: function () {
            openPasswordModal(user);
          }
        })
      ];
      if (!user.isSuper) {
        actions.unshift(LB.toggle(user.enabled, function (checked, input) {
          toggleUser(user, checked, input);
        }, "启用账户"));
      }
      actions.push(h("button", {
        class: "btn btn-danger btn-sm",
        text: "删除",
        disabled: user.isSuper && users.filter(function (item) { return item.isSuper; }).length <= 1,
        onClick: function () {
          deleteUser(user);
        }
      }));

      list.appendChild(h("div", { class: "row" + (user.enabled ? "" : " muted") }, [
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [h("span", { text: user.username })].concat(badges)),
          h("div", {
            class: "row-sub",
            text: "创建于 " + LB.fmtTime(user.createdAt) + " · 最近登录 " + LB.fmtTime(user.lastLoginAt) +
              (user.note ? " · " + user.note : "")
          })
        ]),
        h("div", { class: "row-actions" }, actions)
      ]));
    });
  }

  function openUserModal() {
    var nameInput = h("input", { type: "text", spellcheck: false, autocomplete: "off", placeholder: "3-48 个字符，区分大小写" });
    var passwordInput = h("input", { type: "password", autocomplete: "new-password", placeholder: "至少 8 个字符" });
    var confirmInput = h("input", { type: "password", autocomplete: "new-password" });
    var roleSelect = h("select", {});
    roleSelect.appendChild(h("option", { value: "admin", text: "管理员（仅数据看板）" }));
    roleSelect.appendChild(h("option", { value: "super", text: "超级管理员（全部页面）" }));
    var noteInput = h("input", { type: "text", placeholder: "可选" });

    LB.modal({
      title: "添加管理员",
      subtitle: "管理员只能看到数据看板，无法通过网址跳转访问分组与账号页",
      body: h("div", { class: "form-grid" }, [
        field("账户名", nameInput, null, true),
        field("密码", passwordInput, "至少 8 个字符；首尾空格与换行会被忽略", true),
        field("确认密码", confirmInput, null, true),
        field("角色", roleSelect, null, true),
        field("备注", noteInput)
      ]),
      confirmText: "创建账户",
      onConfirm: async function () {
        // 与服务端一致地忽略首尾空白，保证这里创建的口令一定能登录。
        var password = passwordInput.value.trim();
        if (nameInput.value.trim().length < 3) {
          LB.toast("账户名至少 3 个字符", "error");
          return false;
        }
        if (password.length < 8) {
          LB.toast("密码至少 8 个字符", "error");
          return false;
        }
        if (password !== confirmInput.value.trim()) {
          LB.toast("两次输入的密码不一致", "error");
          return false;
        }
        await LB.request("POST", "/admin/users", {
          user: nameInput.value.trim(),
          password: password,
          role: roleSelect.value,
          note: noteInput.value.trim()
        });
        LB.toast("账户已创建", "ok");
        await loadAll();
      }
    });
  }

  function openPasswordModal(user) {
    var passwordInput = h("input", { type: "password", autocomplete: "new-password", placeholder: "至少 8 个字符" });
    var confirmInput = h("input", { type: "password", autocomplete: "new-password" });

    LB.modal({
      title: "重置「" + user.username + "」的口令",
      subtitle: "重置后该账户的所有会话会立即失效",
      body: h("div", { class: "form-grid" }, [
        field("新密码", passwordInput, "至少 8 个字符；首尾空格与换行会被忽略", true),
        field("确认密码", confirmInput, null, true)
      ]),
      confirmText: "重置口令",
      onConfirm: async function () {
        // 粘贴口令常带尾随空格；这里先按服务端规则归一化再提交。
        var password = passwordInput.value.trim();
        if (password.length < 8) {
          LB.toast("密码至少 8 个字符", "error");
          return false;
        }
        if (password !== confirmInput.value.trim()) {
          LB.toast("两次输入的密码不一致", "error");
          return false;
        }
        await LB.request("POST", "/admin/users/" + encodeURIComponent(user.id) + "/password", {
          password: password,
          confirm: password
        });
        LB.toast("口令已重置，请把新口令交给该账户", "ok");
        await loadAll();
      }
    });
  }

  async function toggleUser(user, enabled, input) {
    input.disabled = true;
    try {
      await LB.request("POST", "/admin/users/" + encodeURIComponent(user.id) + "/enable", { enabled: enabled });
      LB.toast("账户已" + (enabled ? "启用" : "禁用"), "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
      input.checked = !enabled;
    } finally {
      input.disabled = false;
    }
  }

  async function deleteUser(user) {
    if (!window.confirm("删除账户「" + user.username + "」？该账户会立即失去访问权限。")) {
      return;
    }
    try {
      await LB.request("DELETE", "/admin/users/" + encodeURIComponent(user.id));
      LB.toast("账户已删除", "ok");
      await loadAll();
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  // ---------- 账号移除记录 ----------

  function renderRemoved() {
    var list = el("removed-list");
    LB.clear(list);
    if (!removed.length) {
      list.appendChild(h("div", { class: "empty", text: "暂无账号移除记录" }));
      return;
    }
    removed.slice().reverse().slice(0, 40).forEach(function (item) {
      list.appendChild(h("div", { class: "row" }, [
        h("div", { class: "row-main" }, [
          h("div", { class: "row-title" }, [
            h("span", { text: item.name }),
            h("span", { class: "badge warn", text: item.reason })
          ]),
          h("div", {
            class: "row-sub",
            text: "累计消耗 " + LB.fmtMoney(item.usedAmount) + " · " + LB.fmtNumber(item.tokens) +
              " tokens · " + item.keys + " 个 API · " + LB.fmtTime(item.removedAt)
          })
        ])
      ]));
    });
  }

  // ---------- 加载 ----------

  async function loadAll() {
    try {
      var groupRes = await LB.request("GET", "/admin/groups");
      groups = groupRes.data || [];
      var accountRes = await LB.request("GET", "/admin/accounts");
      accounts = accountRes.data || [];
      removed = accountRes.removed || [];
      var userRes = await LB.request("GET", "/admin/users");
      users = userRes.data || [];

      renderGroups();
      renderAccounts();
      renderUsers();
      renderRemoved();

      var totals = accountRes.totals || {};
      if (totals.accounts) {
        el("account-summary").textContent = totals.accounts.total + " 个账号 · " +
          totals.accounts.apiCount + " 个上游 API · 总余额 " + LB.fmtMoney(totals.balance.total) +
          (totals.accounts.unlimited ? " + " + totals.accounts.unlimited + " 个无限额度" : "") +
          (totals.accounts.suspended ? " · " + totals.accounts.suspended + " 个已暂停" : "");
      }

      var stale = accounts.filter(function (item) { return item.checkError; }).length;
      var latest = accounts.reduce(function (acc, item) {
        return item.checkedAt && (!acc || item.checkedAt > acc) ? item.checkedAt : acc;
      }, "");
      el("refresh-status").textContent = (latest ? "最近查询于 " + LB.fmtTime(latest) : "尚未查询过余额") +
        (stale ? " · " + stale + " 个账号查询失败" : " · 余额耗尽的账号会在刷新后自动暂停");
    } catch (err) {
      LB.toast(err.message, "error");
    }
  }

  async function init() {
    // 该页面只对超级管理员开放，服务端也做了同样的门控。
    var session = await LB.loadSession(true);
    if (!session) {
      return;
    }
    LB.nav("manage");

    el("group-create").addEventListener("click", openGroupModal);
    el("account-create").addEventListener("click", openAccountModal);
    el("user-create").addEventListener("click", openUserModal);
    el("refresh-all").addEventListener("click", function (event) {
      refreshAll(event.currentTarget);
    });
    el("refresh-accounts").addEventListener("click", function (event) {
      refreshAll(event.currentTarget);
    });
    el("reload-list").addEventListener("click", function (event) {
      var button = event.currentTarget;
      button.disabled = true;
      loadAll().finally(function () {
        button.disabled = false;
      });
    });

    await loadAll();
  }

  init();
})();
