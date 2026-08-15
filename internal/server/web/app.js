// 前端公共层：主题切换、请求封装、CSRF、DOM 构建、弹窗与格式化。
// CSP 不允许内联脚本，所有逻辑都在独立 .js 文件内。
(function () {
  "use strict";

  var csrfToken = "";
  var session = null;

  var THEME_KEY = "laskah.theme";
  var THEMES = ["auto", "light", "dark"];
  var THEME_LABEL = { auto: "跟随系统", light: "浅色", dark: "深色" };

  function el(id) {
    return document.getElementById(id);
  }

  // h 只接受结构化参数并统一使用 textContent，避免任何 HTML 字符串拼接带来的注入面。
  function h(tag, options, children) {
    var node = document.createElement(tag);
    options = options || {};
    if (options.class) {
      node.className = options.class;
    }
    if (options.text !== undefined && options.text !== null) {
      node.textContent = String(options.text);
    }
    if (options.type) {
      node.type = options.type;
    }
    if (options.value !== undefined) {
      node.value = options.value;
    }
    if (options.placeholder) {
      node.placeholder = options.placeholder;
    }
    if (options.href) {
      node.setAttribute("href", options.href);
    }
    if (options.src) {
      node.setAttribute("src", options.src);
    }
    if (options.alt !== undefined) {
      node.setAttribute("alt", options.alt);
    }
    if (options.title) {
      node.title = options.title;
    }
    if (options.id) {
      node.id = options.id;
    }
    if (options.min !== undefined) {
      node.min = options.min;
    }
    if (options.max !== undefined) {
      node.max = options.max;
    }
    if (options.step !== undefined) {
      node.step = options.step;
    }
    if (options.rows) {
      node.rows = options.rows;
    }
    if (options.maxlength) {
      node.maxLength = options.maxlength;
    }
    if (options.spellcheck === false) {
      node.spellcheck = false;
    }
    if (options.autocomplete) {
      node.autocomplete = options.autocomplete;
    }
    if (options.checked) {
      node.checked = true;
    }
    if (options.disabled) {
      node.disabled = true;
    }
    if (options.hidden) {
      node.hidden = true;
    }
    if (options.ariaLabel) {
      node.setAttribute("aria-label", options.ariaLabel);
    }
    if (options.dataset) {
      Object.keys(options.dataset).forEach(function (key) {
        node.dataset[key] = options.dataset[key];
      });
    }
    if (options.onClick) {
      node.addEventListener("click", options.onClick);
    }
    if (options.onChange) {
      node.addEventListener("change", options.onChange);
    }
    if (options.onInput) {
      node.addEventListener("input", options.onInput);
    }
    (children || []).forEach(function (child) {
      if (child) {
        node.appendChild(child);
      }
    });
    return node;
  }

  function clear(node) {
    while (node && node.firstChild) {
      node.removeChild(node.firstChild);
    }
  }

  // ---------- 主题 ----------

  function storedTheme() {
    try {
      var value = window.localStorage.getItem(THEME_KEY);
      return THEMES.indexOf(value) >= 0 ? value : "auto";
    } catch (err) {
      // 隐私模式下 localStorage 可能不可用，退回跟随系统。
      return "auto";
    }
  }

  function applyTheme(theme) {
    var next = THEMES.indexOf(theme) >= 0 ? theme : "auto";
    document.documentElement.setAttribute("data-theme", next);
    try {
      window.localStorage.setItem(THEME_KEY, next);
    } catch (err) {
      // 存不下也不影响本次会话的显示。
    }
    return next;
  }

  // initTheme 在文档解析早期调用，避免首屏闪烁。
  function initTheme() {
    applyTheme(storedTheme());
  }

  // themeSwitch 渲染三段式分段控件：深色 / 浅色 / 跟随系统。
  function themeSwitch() {
    var current = storedTheme();
    var segment = h("div", { class: "segment", ariaLabel: "主题" });
    THEMES.forEach(function (theme) {
      var button = h("button", {
        class: "segment-item" + (theme === current ? " active" : ""),
        type: "button",
        text: THEME_LABEL[theme],
        title: "主题：" + THEME_LABEL[theme]
      });
      button.addEventListener("click", function () {
        applyTheme(theme);
        Array.prototype.forEach.call(segment.children, function (item) {
          item.className = "segment-item";
        });
        button.className = "segment-item active";
      });
      segment.appendChild(button);
    });
    return segment;
  }

  // ---------- 提示 ----------

  function toast(message, kind) {
    var box = el("toast");
    if (!box) {
      return;
    }
    box.textContent = String(message == null ? "" : message);
    box.className = "toast show" + (kind ? " " + kind : "");
    window.clearTimeout(box.dataset.timer);
    box.dataset.timer = window.setTimeout(function () {
      box.className = "toast";
    }, kind === "error" ? 5200 : 2800);
  }

  // ---------- 请求 ----------

  // request 统一附带同源 Cookie 与 CSRF 头；401 时跳转登录页。
  async function request(method, path, body) {
    var headers = { Accept: "application/json" };
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
    }
    if (csrfToken && method !== "GET") {
      headers["X-CSRF-Token"] = csrfToken;
    }

    var response = await fetch(path, {
      method: method,
      headers: headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body)
    });

    if (response.status === 401) {
      window.location.href = "/login";
      throw new Error("会话已失效，请重新登录");
    }

    var payload = null;
    try {
      payload = await response.json();
    } catch (err) {
      payload = null;
    }
    if (!response.ok) {
      var message = payload && payload.error && payload.error.message ? payload.error.message : "请求失败 (" + response.status + ")";
      throw new Error(message);
    }
    return payload || {};
  }

  // loadSession 拉取当前登录态；需要超管权限的页面可要求 requireSuper。
  async function loadSession(requireSuper) {
    var response = await fetch("/admin/session", { credentials: "same-origin", headers: { Accept: "application/json" } });
    var payload = await response.json();
    if (!payload.authenticated) {
      window.location.href = "/login";
      return null;
    }
    csrfToken = payload.csrfToken || "";
    session = payload;
    if (requireSuper && !payload.isSuper) {
      window.location.href = "/dashboard";
      return null;
    }
    return payload;
  }

  function currentSession() {
    return session;
  }

  async function logout() {
    try {
      await request("POST", "/admin/logout");
    } catch (err) {
      // 忽略注销失败，直接回登录页。
    }
    window.location.href = "/login";
  }

  // ---------- 格式化 ----------

  function fmtMoney(value, currency) {
    var amount = Number(value || 0);
    var symbol = currency === "CNY" ? "¥" : "$";
    if (amount >= 1000) {
      return symbol + amount.toFixed(2);
    }
    return symbol + amount.toFixed(4).replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "");
  }

  // fmtBalance 在没有配置额度查询时显示“无限余额”。
  function fmtBalance(item) {
    if (item && item.unlimited) {
      return "∞ 无限";
    }
    return fmtMoney(item ? item.balance : 0, item ? item.currency : "");
  }

  function fmtNumber(value) {
    return Number(value || 0).toLocaleString("zh-CN");
  }

  function fmtTime(value) {
    if (!value) {
      return "从未";
    }
    var date = new Date(value);
    if (isNaN(date.getTime())) {
      return "未知";
    }
    return date.toLocaleString("zh-CN", { hour12: false });
  }

  // ---------- 组件 ----------

  function stat(label, value, note, tone) {
    return h("div", { class: "stat" }, [
      h("div", { class: "stat-label", text: label }),
      h("div", { class: "stat-value" + (tone ? " " + tone : ""), text: value }),
      note ? h("div", { class: "stat-note", text: note }) : null
    ]);
  }

  function switchRow(label, id, checked, note) {
    var input = h("input", { type: "checkbox", checked: checked, id: id });
    return h("div", { class: "field full" }, [
      h("div", { class: "switch-row" }, [
        h("div", {}, [h("label", { text: label }), note ? h("div", { class: "hint-text", text: note }) : null]),
        h("span", { class: "switch" }, [input, h("span", { class: "switch-track" })])
      ])
    ]);
  }

  // toggle 是不带外层 field 的紧凑开关，用于列表行内。
  function toggle(checked, onChange, label) {
    var input = h("input", { type: "checkbox", checked: checked, ariaLabel: label || "开关" });
    input.addEventListener("change", function () {
      onChange(input.checked, input);
    });
    return h("span", { class: "switch switch-sm" }, [input, h("span", { class: "switch-track" })]);
  }

  function progressBar(ratio, warn) {
    var clamped = Math.max(0, Math.min(1, Number(ratio) || 0));
    var fill = h("span", {});
    fill.style.width = (clamped * 100).toFixed(1) + "%";
    return h("div", { class: "bar" + (warn ? " warn" : "") }, [fill]);
  }

  // brand 渲染左上角花字 Laskah + 猫咪标识。
  function brand() {
    return h("a", { class: "brand", href: "/dashboard", title: "Laskah" }, [
      h("img", { class: "brand-mark", src: "/logo.png", alt: "" }),
      h("span", { class: "brand-word", text: "Laskah" })
    ]);
  }

  // nav 按角色渲染导航：普通管理员看不到「分组与账号」入口。
  function nav(active) {
    var host = el("nav-host");
    if (host) {
      clear(host);
      host.appendChild(brand());
    }

    var container = el("nav-links");
    if (!container) {
      return;
    }
    var isSuper = !session || session.isSuper;
    var links = [{ href: "/dashboard", label: "数据看板", key: "dashboard" }];
    if (isSuper) {
      links.push({ href: "/manage", label: "分组与账号", key: "manage" });
      links.push({ href: "/manage#users", label: "管理员", key: "users" });
    }

    clear(container);
    links.forEach(function (item) {
      container.appendChild(h("a", {
        class: "nav-link" + (item.key === active ? " active" : ""),
        href: item.href,
        text: item.label
      }));
    });
    container.appendChild(themeSwitch());
    if (session && session.user) {
      container.appendChild(h("span", {
        class: "badge " + (session.isSuper ? "info" : ""),
        text: session.user + (session.isSuper ? " · 超管" : " · 管理员")
      }));
    }
    container.appendChild(h("button", { class: "btn btn-quiet", text: "退出", onClick: logout }));
  }

  // ---------- 居中弹窗 ----------

  var openModal = null;

  // modal 打开一个居中弹窗。options: { title, subtitle, body, confirmText, onConfirm, wide }
  // onConfirm 返回 false 时保持弹窗打开（用于校验失败）。
  function modal(options) {
    closeModal();

    var confirmButton = h("button", {
      class: "btn btn-primary",
      type: "button",
      text: options.confirmText || "确定"
    });
    var panel = h("div", { class: "modal-panel" + (options.wide ? " wide" : "") }, [
      h("div", { class: "modal-head" }, [
        h("h3", { text: options.title || "" }),
        options.subtitle ? h("p", { text: options.subtitle }) : null
      ]),
      h("div", { class: "modal-body" }, [options.body]),
      h("div", { class: "modal-foot" }, [
        h("button", { class: "btn", type: "button", text: options.cancelText || "取消", onClick: closeModal }),
        confirmButton
      ])
    ]);
    var overlay = h("div", { class: "modal-overlay" }, [panel]);

    confirmButton.addEventListener("click", async function () {
      confirmButton.disabled = true;
      var label = confirmButton.textContent;
      confirmButton.textContent = "处理中…";
      try {
        var result = await options.onConfirm();
        if (result !== false) {
          closeModal();
        }
      } catch (err) {
        toast(err.message || "操作失败", "error");
      } finally {
        confirmButton.disabled = false;
        confirmButton.textContent = label;
      }
    });
    // 点遮罩空白处关闭弹窗，但要求按下与松开都发生在遮罩本身。
    //
    // 不能只看 click 的 target：在面板里按住鼠标拖选文本、松手时指针已经移出面板，
    // click 会派发到按下点与松开点的共同祖先（也就是遮罩），弹窗于是被当成
    // 「点了背景」直接关掉，用户填好的表单一起丢。改成配对判定 pointerdown /
    // pointerup 就能把这类跨元素拖拽排除在关闭条件之外。
    var pressedOnOverlay = false;
    overlay.addEventListener("pointerdown", function (event) {
      // 只认主键：右键菜单与中键不参与关闭判定。
      pressedOnOverlay = event.target === overlay && event.button === 0;
    });
    overlay.addEventListener("pointercancel", function () {
      pressedOnOverlay = false;
    });
    overlay.addEventListener("pointerup", function (event) {
      var startedOnOverlay = pressedOnOverlay;
      pressedOnOverlay = false;
      if (!startedOnOverlay || event.target !== overlay || event.button !== 0) {
        return;
      }
      // 起止点都在遮罩上但存在选区，说明这是一次拖选，保留弹窗与选中的文本。
      var selection = window.getSelection ? window.getSelection() : null;
      if (selection && !selection.isCollapsed) {
        return;
      }
      closeModal();
    });

    document.body.appendChild(overlay);
    document.body.classList.add("modal-open");
    openModal = overlay;
    window.setTimeout(function () {
      overlay.classList.add("show");
      var first = panel.querySelector("input, select, textarea");
      if (first) {
        first.focus();
      }
    }, 10);
    return { overlay: overlay, panel: panel, close: closeModal };
  }

  function closeModal() {
    if (!openModal) {
      return;
    }
    var target = openModal;
    openModal = null;
    target.classList.remove("show");
    document.body.classList.remove("modal-open");
    window.setTimeout(function () {
      if (target.parentNode) {
        target.parentNode.removeChild(target);
      }
    }, 220);
  }

  document.addEventListener("keydown", function (event) {
    // 输入法组字中按 Esc 只是取消候选词，不应连带关掉弹窗丢掉已填内容。
    if (event.key === "Escape" && !event.isComposing) {
      closeModal();
    }
  });

  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      toast("已复制到剪贴板", "ok");
    } catch (err) {
      toast("复制失败，请手动选择文本", "error");
    }
  }

  initTheme();

  window.LB = {
    el: el,
    h: h,
    clear: clear,
    toast: toast,
    request: request,
    loadSession: loadSession,
    session: currentSession,
    logout: logout,
    fmtMoney: fmtMoney,
    fmtBalance: fmtBalance,
    fmtNumber: fmtNumber,
    fmtTime: fmtTime,
    stat: stat,
    switchRow: switchRow,
    toggle: toggle,
    progressBar: progressBar,
    brand: brand,
    nav: nav,
    modal: modal,
    closeModal: closeModal,
    themeSwitch: themeSwitch,
    initTheme: initTheme,
    copyText: copyText
  };
})();
