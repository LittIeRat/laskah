// /setup：部署后第一步，创建超级管理员并提示用户保存凭据。
(function () {
  "use strict";

  var LB = window.LB;
  var form = document.getElementById("setup-form");
  var errorBox = document.getElementById("setup-error");
  var resultBox = document.getElementById("setup-result");
  var submit = document.getElementById("setup-submit");

  document.getElementById("setup-theme").appendChild(LB.themeSwitch());

  // 已初始化时不允许停留在本页面。
  fetch("/admin/setup", { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (response) { return response.json(); })
    .then(function (payload) {
      if (payload && payload.needsSetup === false) {
        window.location.href = "/login";
      }
    })
    .catch(function () {});

  form.addEventListener("submit", async function (event) {
    event.preventDefault();
    errorBox.textContent = "";

    var user = document.getElementById("setup-user").value.trim();
    // 与服务端一致地忽略首尾空白：保存下来的凭据必须与实际生效的口令一致。
    var password = document.getElementById("setup-password").value.trim();
    var confirm = document.getElementById("setup-confirm").value.trim();

    if (user.length < 3) {
      errorBox.textContent = "账户名至少 3 个字符";
      return;
    }
    if (password.length < 8) {
      errorBox.textContent = "密码至少 8 个字符";
      return;
    }
    if (password !== confirm) {
      errorBox.textContent = "两次输入的密码不一致";
      return;
    }

    submit.disabled = true;
    submit.textContent = "创建中…";
    try {
      var response = await fetch("/admin/setup", {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        credentials: "same-origin",
        body: JSON.stringify({ user: user, password: password, confirm: confirm })
      });
      var payload = null;
      try {
        payload = await response.json();
      } catch (err) {
        payload = null;
      }
      if (!response.ok) {
        errorBox.textContent = (payload && payload.error && payload.error.message) || "创建失败";
        return;
      }

      resultBox.hidden = false;
      LB.clear(resultBox);
      resultBox.appendChild(LB.h("div", { text: "请立即保存以下凭据（此后不再显示）：" }));
      resultBox.appendChild(LB.h("div", { text: "账户: " + user }));
      resultBox.appendChild(LB.h("div", { text: "密码: " + password }));
      resultBox.appendChild(LB.h("div", { text: "角色: 超级管理员（可访问全部页面）" }));
      LB.toast("超级管理员已创建，3 秒后跳转登录页", "ok");
      window.setTimeout(function () {
        window.location.href = "/login";
      }, 3000);
    } catch (err) {
      errorBox.textContent = err.message || "网络错误";
    } finally {
      submit.disabled = false;
      submit.textContent = "创建并保存";
    }
  });
})();
