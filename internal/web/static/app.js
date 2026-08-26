// Progressive enhancement only. Every action on the site works as a plain
// form post; this file makes a few of them less tedious.
(function () {
  "use strict";

  // Keep the stake box and the shown return in step, so a customer is not
  // told a payout that belongs to a different stake.
  var stake = document.querySelector("[data-stake]");
  var returns = document.querySelector("[data-returns]");
  if (stake && returns) {
    var oddsMilli = parseInt(returns.getAttribute("data-odds"), 10);
    var update = function () {
      var btc = parseFloat(stake.value);
      if (!isFinite(btc) || btc < 0 || !isFinite(oddsMilli)) {
        returns.textContent = "—";
        return;
      }
      var sats = Math.round(btc * 1e8);
      // Truncating matches the server, which never pays a partial satoshi.
      var payout = Math.floor((sats * oddsMilli) / 1000);
      returns.textContent = (payout / 1e8).toFixed(8) + " BTC";
    };
    stake.addEventListener("input", update);
    update();
  }

  // Confirm the irreversible ones.
  document.querySelectorAll("[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (event) {
      if (!window.confirm(form.getAttribute("data-confirm"))) {
        event.preventDefault();
      }
    });
  });

  // Copy a deposit address without selecting it by hand.
  document.querySelectorAll("[data-copy]").forEach(function (button) {
    button.addEventListener("click", function () {
      var text = button.getAttribute("data-copy");
      if (!navigator.clipboard) { return; }
      navigator.clipboard.writeText(text).then(function () {
        var original = button.textContent;
        button.textContent = "Copied";
        setTimeout(function () { button.textContent = original; }, 1500);
      });
    });
  });
})();
