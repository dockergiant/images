(function () {
    var USERNAME = 'localadmin';
    var PASSWORD = 'admin123';
    var ATTEMPT_KEY = 'rollAutoLoginAttempt';
    var RETRY_AFTER_MS = 15000;

    function lastAttempt() {
        try {
            return parseInt(window.sessionStorage.getItem(ATTEMPT_KEY) || '0', 10);
        } catch (e) {
            return 0;
        }
    }

    function rememberAttempt() {
        try {
            window.sessionStorage.setItem(ATTEMPT_KEY, String(Date.now()));
        } catch (e) {}
    }

    function showNotice(form, reason) {
        var notice = document.createElement('div');
        notice.style.cssText = 'margin:0 0 12px;padding:10px 12px;border:1px solid #e0b252;background:#fdf6e3;color:#333;font-size:12px;line-height:1.5;text-align:left';
        notice.innerHTML = '<strong>RollDev auto-login could not sign in as ' + USERNAME + '.</strong><br>'
            + 'Run <code>roll setup-autologin</code> and reload this page, or log in yourself: the fields are filled in.<br>'
            + 'To turn auto-login off, set <code>ROLL_ADMIN_AUTOLOGIN=0</code> in .env.roll and run <code>roll env up</code>.'
            + (reason ? '<br><br>Magento said: <em></em>' : '');
        if (reason) {
            notice.querySelector('em').textContent = reason;
        }
        form.insertBefore(notice, form.firstChild);
    }

    function run() {
        var form = document.getElementById('loginForm');
        var username = document.getElementById('username');
        var password = document.getElementById('login');

        if (document.body.id !== 'page-login' || !form || !username || !password) {
            return;
        }

        username.value = USERNAME;
        password.value = PASSWORD;

        var error = document.querySelector('.error-msg');
        // A failed login reloads this page, so a recent attempt means the credentials did not work
        if (error || Date.now() - lastAttempt() < RETRY_AFTER_MS) {
            showNotice(form, error ? error.textContent.trim() : '');
            return;
        }

        console.info('RollDev auto-login: signing in as ' + USERNAME + ' (ROLL_ADMIN_AUTOLOGIN=0 in .env.roll turns this off)');
        rememberAttempt();
        HTMLFormElement.prototype.submit.call(form);
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', run);
    } else {
        run();
    }
})();
