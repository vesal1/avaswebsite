// The Mines board's front end.
//
// The layout was fixed by the committed seed before the first tile was
// touched; this file animates reveals the server has already resolved and
// sends up which tile was tapped, nothing more.
(function () {
  'use strict';

  var root = document.getElementById('mines');
  if (!root) return;
  var csrf = root.dataset.csrf;
  var LADDERS = JSON.parse(root.dataset.ladders);

  var grid = document.getElementById('mines-grid');
  var setup = document.getElementById('mines-setup');
  var countSelect = document.getElementById('mines-count');
  var stakeSelect = document.getElementById('mines-stake');
  var startButton = document.getElementById('mines-start');
  var cashoutButton = document.getElementById('mines-cashout');
  var ladderList = document.getElementById('mines-ladder');
  var message = document.getElementById('mines-message');
  var winLine = document.getElementById('mines-win');
  var balance = document.getElementById('mines-balance');
  var banner = document.getElementById('mines-banner');
  var busy = false;

  var PARTICLE_COLORS = ['#ffcd3c', '#ff9f1a', '#35d46a', '#4aa8ff', '#ffffff'];

  // Build the 25 tiles once.
  var tiles = [];
  for (var i = 0; i < 25; i++) {
    var tile = document.createElement('button');
    tile.type = 'button';
    tile.className = 'mtile';
    tile.dataset.cell = i;
    tile.setAttribute('aria-label', 'Tile ' + (i + 1));
    grid.appendChild(tile);
    tiles.push(tile);
  }

  function fmtMult(x10000) {
    return (x10000 / 10000).toFixed(2).replace(/\.00$/, '') + 'x';
  }

  function renderLadder(mineCount, reveals) {
    var ladder = LADDERS[mineCount] || [];
    ladderList.textContent = '';
    for (var step = 1; step < ladder.length; step++) {
      var item = document.createElement('li');
      var label = document.createElement('span');
      label.textContent = step + (step === 1 ? ' tile' : ' tiles');
      var value = document.createElement('strong');
      value.textContent = fmtMult(ladder[step]);
      item.appendChild(label);
      item.appendChild(value);
      if (step === reveals) item.classList.add('current');
      if (step < reveals) item.classList.add('passed');
      ladderList.appendChild(item);
    }
    var current = ladderList.querySelector('.current');
    if (current) current.scrollIntoView({ block: 'nearest' });
  }

  function resetBoard() {
    for (var i = 0; i < tiles.length; i++) {
      tiles[i].className = 'mtile';
      tiles[i].textContent = '';
      tiles[i].disabled = false;
    }
    banner.textContent = '';
  }

  function confetti(count) {
    for (var i = 0; i < count; i++) {
      var piece = document.createElement('span');
      piece.className = 'confetti-piece';
      piece.style.left = (Math.random() * 100) + '%';
      piece.style.background = PARTICLE_COLORS[(Math.random() * PARTICLE_COLORS.length) | 0];
      piece.style.setProperty('--drift', ((Math.random() - 0.5) * 160) + 'px');
      piece.style.setProperty('--spin', (360 + Math.random() * 540) + 'deg');
      piece.style.setProperty('--fall', (1.4 + Math.random() * 1.2) + 's');
      root.appendChild(piece);
      (function (node) { setTimeout(function () { node.remove(); }, 2800); })(piece);
    }
  }

  function render(view) {
    if (!view.open && !view.status) {
      // No board: setup mode, tiles inert.
      resetBoard();
      for (var i = 0; i < tiles.length; i++) tiles[i].disabled = true;
      setup.hidden = false;
      cashoutButton.hidden = true;
      renderLadder(parseInt(countSelect.value, 10), 0);
      return;
    }

    setup.hidden = view.open;
    renderLadder(view.mine_count, view.reveals);

    var revealed = {};
    (view.revealed || []).forEach(function (cell) { revealed[cell] = true; });
    var mineSet = {};
    (view.mines || []).forEach(function (cell) { mineSet[cell] = true; });

    for (var i = 0; i < tiles.length; i++) {
      var tile = tiles[i];
      if (revealed[i]) {
        if (!tile.classList.contains('gem')) {
          tile.className = 'mtile gem';
          tile.textContent = '💎';
        }
        tile.disabled = true;
      } else if (view.hit === i) {
        tile.className = 'mtile boom';
        tile.textContent = '💥';
        tile.disabled = true;
      } else if (!view.open && mineSet[i]) {
        tile.className = 'mtile mine-shown';
        tile.textContent = '💣';
        tile.disabled = true;
      } else if (!view.open) {
        tile.className = 'mtile';
        tile.textContent = '';
        tile.disabled = true;
      } else {
        tile.className = 'mtile';
        tile.textContent = '';
        tile.disabled = false;
      }
    }

    if (view.open) {
      banner.textContent = '';
      winLine.textContent = '';
      winLine.className = 'win-line';
      if (view.reveals > 0) {
        cashoutButton.hidden = false;
        cashoutButton.textContent = 'Cash out ' + view.cash_out_btc + ' BTC';
        cashoutButton.classList.add('pulse');
        var next = view.next_mult_x10000 ? ' Next tile: ' + fmtMult(view.next_mult_x10000) + '.' : '';
        message.textContent = view.reveals + ' safe — worth ' + fmtMult(view.mult_x10000) + '.' + next;
      } else {
        cashoutButton.hidden = true;
        message.textContent = view.mine_count + ' mines are down there. Pick a tile.';
      }
    } else {
      cashoutButton.hidden = true;
      cashoutButton.classList.remove('pulse');
      var result = document.createElement('span');
      if (view.status === 'won') {
        result.className = 'result-banner won';
        result.textContent = view.cashed_out ? 'Cashed out' : 'Ladder cleared';
        winLine.className = 'win-line good';
        winLine.textContent = 'Paid ' + view.win_btc + ' BTC';
        confetti(50);
        root.classList.add('celebrating');
        setTimeout(function () { root.classList.remove('celebrating'); }, 2200);
      } else {
        result.className = 'result-banner lost';
        result.textContent = 'Boom';
        winLine.className = 'win-line bad';
        winLine.textContent = '';
      }
      banner.textContent = '';
      banner.appendChild(result);
      message.textContent = 'Round #' + view.nonce + ' — checkable once you publish your seed. Another board?';
    }
    if (view.balance_btc) balance.textContent = view.balance_btc;
  }

  function post(path, params) {
    var body = new URLSearchParams(params || {});
    body.set('csrf', csrf);
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (response) {
      return response.json().then(function (payload) {
        if (!response.ok) {
          var err = new Error(payload.error || 'That did not work.');
          err.status = response.status;
          throw err;
        }
        return payload;
      });
    });
  }

  function guard(work) {
    if (busy) return;
    busy = true;
    work().catch(function (err) {
      message.textContent = err.message;
      // Out of step with the server — a finished round acted on, or a live
      // one dealt over. The server's view of the round is the truth, so
      // fetch it and draw that.
      if (err.status === 409) {
        fetch('/casino/mines/state', { credentials: 'same-origin' })
          .then(function (response) { return response.json(); })
          .then(render)
          .catch(function () {});
      }
    }).finally(function () { busy = false; });
  }

  setup.addEventListener('submit', function (event) {
    event.preventDefault();
    guard(function () {
      resetBoard();
      return post('/casino/mines/start', {
        mines: countSelect.value,
        stake: stakeSelect.value
      }).then(render);
    });
  });

  countSelect.addEventListener('change', function () {
    renderLadder(parseInt(countSelect.value, 10), 0);
  });

  grid.addEventListener('click', function (event) {
    var tile = event.target.closest('.mtile');
    if (!tile || tile.disabled) return;
    guard(function () {
      return post('/casino/mines/reveal', { cell: tile.dataset.cell }).then(render);
    });
  });

  cashoutButton.addEventListener('click', function () {
    guard(function () {
      return post('/casino/mines/cashout').then(render);
    });
  });

  // Resume whatever board is live.
  fetch('/casino/mines/state', { credentials: 'same-origin' })
    .then(function (response) { return response.json(); })
    .then(function (view) {
      if (view.open) message.textContent = 'You have a board in play — picking it back up.';
      render(view);
    })
    .catch(function () { render({ open: false }); });
})();
