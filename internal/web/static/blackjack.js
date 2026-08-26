// The blackjack table's front end.
//
// Renders hands the server has already dealt and settled. The deck order was
// fixed by the committed seed before the first card showed; the only thing
// this file sends upward is which button was pressed.
(function () {
  'use strict';

  var root = document.getElementById('bj');
  if (!root) return;
  var csrf = root.dataset.csrf;

  var dealerCards = document.getElementById('bj-dealer-cards');
  var dealerTotal = document.getElementById('bj-dealer-total');
  var handsBox = document.getElementById('bj-hands');
  var banner = document.getElementById('bj-banner');
  var message = document.getElementById('bj-message');
  var winLine = document.getElementById('bj-win');
  var balance = document.getElementById('bj-balance');
  var hintPill = document.getElementById('bj-hint');
  var hintAction = document.getElementById('bj-hint-action');
  var actionsBar = document.getElementById('bj-actions');
  var dealForm = document.getElementById('bj-deal-form');
  var dealButton = document.getElementById('bj-deal');
  var stakeSelect = document.getElementById('bj-stake');
  var busy = false;

  function cardNode(card) {
    var node = document.createElement('div');
    node.className = 'pcard' + (card.red ? ' red' : '');
    var corner = document.createElement('span');
    corner.className = 'corner';
    corner.textContent = card.rank + '\n' + card.suit;
    var pip = document.createElement('span');
    pip.className = 'pip';
    pip.textContent = card.suit;
    node.appendChild(corner);
    node.appendChild(pip);
    return node;
  }

  function backNode() {
    var node = document.createElement('div');
    node.className = 'pcard back';
    return node;
  }

  // syncCards keeps existing card elements in place so only new deals
  // animate in; a full rebuild would replay every entrance on every action.
  function syncCards(box, cards, holeHidden) {
    var want = cards.length + (holeHidden ? 1 : 0);
    var haveHole = box.querySelector('.pcard.back') !== null;
    if (box.childElementCount > want || (haveHole && !holeHidden)) {
      box.textContent = '';
    }
    var existing = box.childElementCount - (box.querySelector('.pcard.back') ? 1 : 0);
    for (var i = existing; i < cards.length; i++) {
      var hole = box.querySelector('.pcard.back');
      box.insertBefore(cardNode(cards[i]), hole);
    }
    if (holeHidden && !box.querySelector('.pcard.back')) {
      box.appendChild(backNode());
    }
  }

  function totalBadge(hand) {
    var badge = document.createElement('span');
    badge.className = 'total-badge';
    var label = String(hand.total);
    if (hand.soft) label = 'soft ' + label;
    if (hand.busted) { badge.classList.add('bust'); label += ' · bust'; }
    if (hand.total === 21 && hand.cards.length === 2 && hand.bet_units === 1) {
      badge.classList.add('blackjack');
    }
    if (hand.bet_units > 1) label += ' · doubled';
    badge.textContent = label;
    return badge;
  }

  function render(view) {
    if (!view.open && !view.player) {
      // Nothing in play: table waits for a deal.
      dealerCards.textContent = '';
      handsBox.textContent = '';
      banner.textContent = '';
      dealerTotal.hidden = true;
      actionsBar.hidden = true;
      hintPill.hidden = true;
      dealForm.hidden = false;
      return;
    }

    dealForm.hidden = view.open;
    syncCards(dealerCards, view.dealer || [], view.dealer_hole_hidden);
    if (view.dealer_total) {
      dealerTotal.hidden = false;
      dealerTotal.textContent = view.dealer_total > 21 ? view.dealer_total + ' · bust' : view.dealer_total;
      dealerTotal.classList.toggle('bust', view.dealer_total > 21);
    } else {
      dealerTotal.hidden = true;
    }

    // Hands: rebuild the frames when the count changes (a split), otherwise
    // sync in place.
    var hands = view.player || [];
    if (handsBox.childElementCount !== hands.length) {
      handsBox.textContent = '';
      for (var i = 0; i < hands.length; i++) {
        var frame = document.createElement('div');
        frame.className = 'bj-hand';
        var cardsBox = document.createElement('div');
        cardsBox.className = 'bj-cards';
        frame.appendChild(cardsBox);
        handsBox.appendChild(frame);
      }
    }
    for (var h = 0; h < hands.length; h++) {
      var frameNode = handsBox.children[h];
      frameNode.classList.toggle('active', !!hands[h].active);
      syncCards(frameNode.querySelector('.bj-cards'), hands[h].cards, false);
      var old = frameNode.querySelector('.total-badge');
      if (old) old.remove();
      frameNode.appendChild(totalBadge(hands[h]));
    }

    // Actions.
    if (view.open && view.actions && view.actions.length) {
      actionsBar.hidden = false;
      var buttons = actionsBar.querySelectorAll('button');
      for (var b = 0; b < buttons.length; b++) {
        buttons[b].hidden = view.actions.indexOf(buttons[b].dataset.action) < 0;
        buttons[b].disabled = busy;
      }
    } else {
      actionsBar.hidden = true;
    }
    if (view.open && view.advice) {
      hintPill.hidden = false;
      hintAction.textContent = view.advice;
    } else {
      hintPill.hidden = true;
    }

    // Outcome.
    banner.textContent = '';
    winLine.textContent = '';
    winLine.className = 'win-line';
    if (!view.open && view.status) {
      var result = document.createElement('span');
      result.className = 'result-banner ' + view.status;
      if (view.dealer_blackjack) {
        result.textContent = view.player_blackjack ? 'Both blackjack — push' : 'Dealer blackjack';
      } else if (view.player_blackjack) {
        result.textContent = 'Blackjack!';
      } else {
        result.textContent = view.status === 'won' ? 'You win' : view.status === 'pushed' ? 'Push' : 'Dealer wins';
      }
      banner.appendChild(result);
      if (view.status === 'won') {
        winLine.className = 'win-line good';
        winLine.textContent = 'Paid ' + view.win_btc + ' BTC';
        root.classList.add('celebrating');
        setTimeout(function () { root.classList.remove('celebrating'); }, 2200);
      } else if (view.status === 'pushed') {
        winLine.textContent = 'Stake returned';
      }
      message.textContent = 'Round #' + view.nonce + ' — checkable once you publish your seed. Deal again?';
    } else if (view.open) {
      message.textContent = 'Your move.';
    }
    if (view.balance_btc) balance.textContent = view.balance_btc;
  }

  function post(path, params) {
    var body = new URLSearchParams(params);
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
    dealButton.disabled = true;
    work().catch(function (err) {
      message.textContent = err.message;
      // Out of step with the server — a finished round acted on, or a live
      // one dealt over. The server's view of the round is the truth, so
      // fetch it and draw that.
      if (err.status === 409) {
        fetch('/casino/blackjack/state', { credentials: 'same-origin' })
          .then(function (response) { return response.json(); })
          .then(render)
          .catch(function () {});
      }
    }).finally(function () {
      busy = false;
      dealButton.disabled = false;
      var buttons = actionsBar.querySelectorAll('button');
      for (var b = 0; b < buttons.length; b++) buttons[b].disabled = false;
    });
  }

  dealForm.addEventListener('submit', function (event) {
    event.preventDefault();
    guard(function () {
      return post('/casino/blackjack/deal', { stake: stakeSelect.value }).then(render);
    });
  });

  actionsBar.addEventListener('click', function (event) {
    var button = event.target.closest('button[data-action]');
    if (!button) return;
    guard(function () {
      return post('/casino/blackjack/act', { action: button.dataset.action }).then(render);
    });
  });

  // Resume whatever is on the table.
  fetch('/casino/blackjack/state', { credentials: 'same-origin' })
    .then(function (response) { return response.json(); })
    .then(function (view) {
      if (view.open) {
        message.textContent = 'You have a hand in play — picking it back up.';
      }
      render(view);
    })
    .catch(function () {});
})();
