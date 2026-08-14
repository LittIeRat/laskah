// 登录页：提交账户口令，成功后由服务端下发 HttpOnly 会话 Cookie。
(function () {
  "use strict";
  var form = document.getElementById("login-form");
  var errorBox = document.getElementById("login-error");
  var submit = document.getElementById("login-submit");

  document.getElementById("login-theme").appendChild(window.LB.themeSwitch());

  // 已登录时按角色跳到对应首页；未初始化时去 /setup。
  fetch("/admin/session", { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (response) { return response.json(); })
    .then(function (payload) {
      if (payload && payload.authenticated) {
        window.location.href = payload.home || "/dashboard";
      }
    })
    .catch(function () {});

  form.addEventListener("submit", async function (event) {
    event.preventDefault();
    errorBox.textContent = "";
    submit.disabled = true;
    submit.textContent = "登录中…";

    try {
      var response = await fetch("/admin/login", {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        credentials: "same-origin",
        body: JSON.stringify({
          user: document.getElementById("login-user").value,
          password: document.getElementById("login-password").value
        })
      });
      var payload = null;
      try {
        payload = await response.json();
      } catch (err) {
        payload = null;
      }
      if (response.status === 409) {
        window.location.href = "/setup";
        return;
      }
      if (!response.ok) {
        var message = payload && payload.error && payload.error.message ? payload.error.message : "登录失败";
        errorBox.textContent = message;
        return;
      }
      window.location.href = (payload && payload.home) || "/dashboard";
    } catch (err) {
      errorBox.textContent = err.message || "网络错误";
    } finally {
      submit.disabled = false;
      submit.textContent = "登录";
    }
  });
})();
