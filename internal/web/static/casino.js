// The slot machine's front end.
//
// It draws reels and animates them. It does not decide anything: the outcome
// arrives from the server, already settled and already recorded, and this file
// only reveals what the seed had fixed before the request was sent. There is
// no "near miss" logic here and nowhere to put any — the page is handed the
// final window and shows it.
(function () {
  'use strict';

  var machine = document.querySelector('.machine');
  if (!machine) return;

  var form = document.getElementById('spin-form');
  var button = document.getElementById('spin-button');
  var stake = document.getElementById('stake');
  var message = document.getElementById('spin-message');
  var winLine = document.getElementById('spin-win');
  var balance = document.getElementById('balance');
  var reelBox = document.getElementById('reels');

  var reelCount = parseInt(machine.dataset.reels, 10);
  var rowCount = parseInt(machine.dataset.rows, 10);
  var spinning = false;

  // A pool of faces used only for the blur while a reel is turning. What
  // finally lands comes from the server's window, never from here.
  var faces = [];
  document.querySelectorAll('.paytable-symbol, .symbol').forEach(function (node) {
    var glyph = node.textContent.trim();
    if (glyph && faces.indexOf(glyph) === -1) faces.push(glyph);
  });
  if (faces.length === 0) faces = ['⭐', '₿', '🎰', '🔑', '🪙'];

  function cells(reel) {
    return reelBox.querySelectorAll('.reel[data-reel="' + reel + '"] .cell');
  }

  function scramble(reel) {
    cells(reel).forEach(function (cell) {
      cell.textContent = faces[Math.floor(Math.random() * faces.length)];
    });
  }

  function settle(reel, symbols) {
    var nodes = cells(reel);
    for (var row = 0; row < nodes.length; row++) {
      nodes[row].textContent = symbols[row] || '';
      nodes[row].classList.remove('lit');
    }
  }

  function light(spin) {
    (spin.wins || []).forEach(function (win) {
      (win.positions || []).forEach(function (position) {
        var nodes = cells(position.reel);
        if (nodes[position.row]) nodes[position.row].classList.add('lit');
      });
    });
  }

  // glyphs maps a server window (symbol ids) onto what to draw, using the
  // paytable already on the page.
  var glyphById = {};
  document.querySelectorAll('[data-symbol-id]').forEach(function (node) {
    glyphById[node.dataset.symbolId] = node.textContent.trim();
  });

  function windowGlyphs(spin) {
    return (spin.window || []).map(function (reel) {
      return reel.map(function (id) { return glyphById[id] || '?'; });
    });
  }

  function showSpin(spin, label) {
    return new Promise(function (resolve) {
      var grid = windowGlyphs(spin);
      var reel = 0;
      message.textContent = label;

      var blur = setInterval(function () {
        for (var i = reel; i < reelCount; i++) scramble(i);
      }, 60);

      function stop() {
        if (reel >= reelCount) {
          clearInterval(blur);
          light(spin);
          setTimeout(resolve, 260);
          return;
        }
        settle(reel, grid[reel] || []);
        reel++;
        setTimeout(stop, 180);
      }
      setTimeout(stop, 380);
    });
  }

  async function reveal(round) {
    await showSpin(round.base, 'Spinning…');
    if (round.free && round.free.length) {
      for (var i = 0; i < round.free.length; i++) {
        message.textContent = 'Free spin ' + (i + 1) + ' of ' + round.free.length;
        await showSpin(round.free[i], message.textContent);
      }
    }
  }

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (spinning) return;
    spinning = true;
    button.disabled = true;
    winLine.textContent = '';
    winLine.className = 'win-line';

    var body = new URLSearchParams();
    body.set('csrf', machine.dataset.csrf);
    body.set('stake', stake.value);

    fetch(form.action, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (response) {
      return response.json().then(function (payload) {
        return { ok: response.ok, payload: payload };
      });
    }).then(async function (result) {
      if (!result.ok) {
        message.textContent = result.payload.error || 'That spin could not be taken.';
        winLine.className = 'win-line bad';
        return;
      }
      var payload = result.payload;
      await reveal(payload.round);

      balance.textContent = payload.balance_btc;
      if (payload.win_sat > 0) {
        winLine.textContent = 'Paid ' + payload.win_btc + ' BTC';
        winLine.className = 'win-line good';
        message.textContent = 'Spin #' + payload.nonce + ' — checkable once you publish your seed.';
      } else {
        winLine.textContent = '';
        message.textContent = 'No win on spin #' + payload.nonce + '. Pick a stake and spin.';
      }
    }).catch(function () {
      message.textContent = 'The connection dropped. Check your spin history before spinning again.';
      winLine.className = 'win-line bad';
    }).finally(function () {
      spinning = false;
      button.disabled = false;
    });
  });
})();
