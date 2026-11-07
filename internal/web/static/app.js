// Progressive enhancement only: tabs and copy buttons. Pages work without JS.
(function () {
  var root = document.documentElement;
  root.classList.add("js");

  document.querySelectorAll("[data-tabs]").forEach(function (box) {
    var buttons = box.querySelectorAll(":scope > .ctabs-list > [role=tab]");
    buttons.forEach(function (btn) {
      btn.addEventListener("click", function () {
        buttons.forEach(function (b) {
          var on = b === btn;
          b.setAttribute("aria-selected", on ? "true" : "false");
          var panel = document.getElementById(b.dataset.tab);
          if (panel) {
            if (on) panel.removeAttribute("data-hidden");
            else panel.setAttribute("data-hidden", "");
          }
        });
      });
    });
  });

  document.querySelectorAll("[data-copy]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var code = btn.parentElement.querySelector("code");
      if (!code) return;
      var text = code.innerText;
      var done = function () {
        var old = btn.textContent;
        btn.textContent = "Copied";
        setTimeout(function () { btn.textContent = old; }, 1200);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {});
      } else {
        var ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand("copy"); done(); } catch (e) {}
        document.body.removeChild(ta);
      }
    });
  });
})();
