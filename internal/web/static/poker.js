// Poker table client.
//
// Two jobs: keep the table drawn from the server's event stream, and run the
// peer-to-peer video mesh between the players who have turned their camera on.
//
// The video never touches our servers. Browsers connect directly to each
// other; all that passes through the site is the handshake needed to set that
// up. Nothing is recorded anywhere in this file, and there is deliberately no
// code path that could.
(function () {
  "use strict";

  var root = document.getElementById("poker-root");
  if (!root) { return; }

  var tableID = root.getAttribute("data-table");
  var mySeat = parseInt(root.getAttribute("data-seat"), 10);
  var csrf = root.getAttribute("data-csrf");
  var videoAllowed = root.getAttribute("data-video") === "1";

  var seatsEl = document.getElementById("seats");
  var boardEl = document.getElementById("board");
  var potEl = document.getElementById("pot");
  var stageTag = document.getElementById("stage-tag");
  var logEl = document.getElementById("hand-log");
  var actionPanel = document.getElementById("action-panel");
  var actionButtons = document.getElementById("action-buttons");
  var actionError = document.getElementById("action-error");
  var raiseField = document.getElementById("raise-field");
  var raiseInput = document.getElementById("raise-amount");
  var raiseDisplay = document.getElementById("raise-display");
  var clockEl = document.getElementById("clock");
  var commitmentEl = document.getElementById("commitment");

  var latest = null;

  // ------------------------------------------------------------- formatting
  function btc(sats) {
    var value = Number(sats || 0) / 1e8;
    return value.toFixed(8);
  }

  function cardEl(text, small) {
    var span = document.createElement("span");
    span.className = "card" + (small ? " small" : "");
    if (text === null) {
      span.className += " back";
      span.textContent = "";
      return span;
    }
    span.textContent = text;
    // Hearts and diamonds read red, as on a real deck.
    if (text.length === 2 && (text[1] === "h" || text[1] === "d")) {
      span.className += " red";
    }
    return span;
  }

  // ---------------------------------------------------------------- drawing
  function render(view) {
    latest = view;

    stageTag.textContent = view.stage.charAt(0).toUpperCase() + view.stage.slice(1);
    potEl.textContent = "Pot " + btc(view.pot_sat) + " BTC";

    boardEl.replaceChildren();
    (view.board || []).forEach(function (card) { boardEl.appendChild(cardEl(card)); });

    seatsEl.replaceChildren();
    (view.seats || []).forEach(function (seat) { seatsEl.appendChild(renderSeat(seat)); });

    if (logEl) {
      logEl.textContent = (view.log || []).join("\n");
      logEl.scrollTop = logEl.scrollHeight;
    }
    if (commitmentEl && view.commitment) { commitmentEl.textContent = view.commitment; }

    renderActions(view);
    if (videoAllowed) { reconcileMesh(view); }
  }

  function renderSeat(seat) {
    var el = document.createElement("div");
    el.className = "seat";
    el.id = "seat-" + seat.seat;
    if (!seat.occupied) { el.className += " empty"; }
    if (seat.is_turn) { el.className += " turn"; }
    if (seat.is_you) { el.className += " you"; }
    if (seat.status === "folded") { el.className += " folded"; }

    var who = document.createElement("div");
    who.className = "who";
    var name = document.createElement("span");
    name.className = "name";
    name.textContent = seat.occupied ? seat.name : "Seat " + seat.seat + " — open";
    var stack = document.createElement("span");
    stack.className = "stack";
    stack.textContent = seat.occupied ? btc(seat.stack_sat) : "";
    who.appendChild(name);
    who.appendChild(stack);
    el.appendChild(who);

    // A slot for this seat's video, filled in by the mesh when they share.
    if (seat.occupied && seat.video_consent) {
      var video = document.getElementById("video-" + seat.seat);
      if (!video) {
        video = document.createElement("video");
        video.id = "video-" + seat.seat;
        video.autoplay = true;
        video.playsInline = true;
        // Never play your own audio back at yourself.
        video.muted = seat.is_you;
      }
      el.appendChild(video);
    }

    var cards = document.createElement("div");
    cards.className = "cards";
    if (seat.cards && seat.cards.length) {
      seat.cards.forEach(function (card) { cards.appendChild(cardEl(card, true)); });
    } else if (seat.has_cards) {
      cards.appendChild(cardEl(null, true));
      cards.appendChild(cardEl(null, true));
    }
    el.appendChild(cards);

    var badges = document.createElement("div");
    badges.className = "badges";
    if (seat.is_button) { badges.appendChild(tag("Button", "")); }
    if (seat.status === "all_in") { badges.appendChild(tag("All in", "warn")); }
    if (seat.status === "folded") { badges.appendChild(tag("Folded", "")); }
    if (seat.status === "sitting_out") { badges.appendChild(tag("Sitting out", "")); }
    if (seat.timed_out) { badges.appendChild(tag("Timed out", "warn")); }
    if (seat.video_consent) { badges.appendChild(tag("Live", "good")); }
    el.appendChild(badges);

    if (seat.committed_sat) {
      var committed = document.createElement("div");
      committed.className = "committed";
      committed.textContent = "Bet " + btc(seat.committed_sat) + " BTC";
      el.appendChild(committed);
    }
    return el;
  }

  function tag(text, kind) {
    var span = document.createElement("span");
    span.className = "tag" + (kind ? " " + kind : "");
    span.textContent = text;
    return span;
  }

  // ---------------------------------------------------------------- actions
  function renderActions(view) {
    var actions = view.legal_actions || [];
    if (!actions.length) {
      actionPanel.hidden = true;
      clockEl.textContent = "";
      return;
    }
    actionPanel.hidden = false;
    clockEl.textContent = view.seconds_left > 0 ? view.seconds_left + "s" : "";

    actionButtons.replaceChildren();
    var wantsAmount = actions.indexOf("bet") >= 0 || actions.indexOf("raise") >= 0;

    actions.forEach(function (action) {
      var button = document.createElement("button");
      if (action === "fold") { button.className = "secondary"; }
      button.type = "button";
      button.textContent = label(action, view);
      button.addEventListener("click", function () {
        var amount = 0;
        if (action === "bet" || action === "raise") {
          amount = parseInt(raiseInput.value, 10) || view.min_raise_to_sat;
        }
        send(action, amount);
      });
      actionButtons.appendChild(button);
    });

    if (wantsAmount && view.max_raise_to_sat > view.min_raise_to_sat) {
      raiseField.hidden = false;
      raiseInput.min = view.min_raise_to_sat;
      raiseInput.max = view.max_raise_to_sat;
      if (!raiseInput.value ||
          Number(raiseInput.value) < view.min_raise_to_sat ||
          Number(raiseInput.value) > view.max_raise_to_sat) {
        raiseInput.value = view.min_raise_to_sat;
      }
      raiseDisplay.textContent = btc(raiseInput.value);
    } else {
      raiseField.hidden = true;
    }
  }

  function label(action, view) {
    if (action === "call") { return "Call " + btc(view.call_amount_sat); }
    if (action === "check") { return "Check"; }
    if (action === "fold") { return "Fold"; }
    if (action === "bet") { return "Bet"; }
    if (action === "raise") { return "Raise"; }
    return action;
  }

  if (raiseInput) {
    raiseInput.addEventListener("input", function () {
      raiseDisplay.textContent = btc(raiseInput.value);
    });
  }

  function send(action, amount) {
    actionError.textContent = "";
    var body = new URLSearchParams();
    body.set("csrf", csrf);
    body.set("action", action);
    body.set("amount", String(amount || 0));

    fetch("/poker/tables/" + tableID + "/act", {
      method: "POST", body: body, headers: { "Accept": "application/json" }
    }).then(function (response) {
      return response.json().then(function (data) {
        if (!response.ok) { actionError.textContent = data.error || "That action was refused."; }
      });
    }).catch(function () {
      actionError.textContent = "Could not reach the table. Check your connection.";
    });
  }

  // ----------------------------------------------------------- event stream
  var stream = new EventSource("/poker/tables/" + tableID + "/stream");
  stream.addEventListener("state", function (event) {
    try { render(JSON.parse(event.data)); } catch (err) { /* ignore a bad frame */ }
  });
  stream.addEventListener("signal", function (event) {
    try { onSignal(JSON.parse(event.data)); } catch (err) { /* ignore */ }
  });

  // ------------------------------------------------------------- video mesh
  //
  // A mesh rather than a media server: at six seats that is fifteen peer
  // connections, which a browser handles comfortably, and it means the
  // operator never carries anybody's video. Beyond about eight participants a
  // mesh stops scaling and an SFU would be needed — with the privacy cost that
  // implies.
  var peers = {};        // seat -> RTCPeerConnection
  var localStream = null;
  var sharing = false;

  var iceServers = [{ urls: "stun:stun.l.google.com:19302" }];

  var videoToggle = document.getElementById("video-toggle");
  var muteToggle = document.getElementById("mute-toggle");
  var cameraToggle = document.getElementById("camera-toggle");
  var videoStatus = document.getElementById("video-status");

  if (videoToggle) {
    videoToggle.addEventListener("click", function () {
      if (sharing) { stopSharing(); } else { startSharing(); }
    });
  }
  if (muteToggle) {
    muteToggle.addEventListener("click", function () {
      var track = localStream && localStream.getAudioTracks()[0];
      if (!track) { return; }
      track.enabled = !track.enabled;
      muteToggle.textContent = track.enabled ? "Mute" : "Unmute";
    });
  }
  if (cameraToggle) {
    cameraToggle.addEventListener("click", function () {
      var track = localStream && localStream.getVideoTracks()[0];
      if (!track) { return; }
      track.enabled = !track.enabled;
      cameraToggle.textContent = track.enabled ? "Hide camera" : "Show camera";
    });
  }

  function startSharing() {
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      videoStatus.textContent = "This browser cannot share a camera.";
      return;
    }
    navigator.mediaDevices.getUserMedia({
      video: { width: 320, height: 240 },
      audio: { echoCancellation: true, noiseSuppression: true }
    }).then(function (stream) {
      localStream = stream;
      sharing = true;
      videoToggle.textContent = "Turn off camera & mic";
      muteToggle.hidden = false;
      cameraToggle.hidden = false;
      videoStatus.textContent = "Sharing with the table. Nothing is recorded.";
      attachLocalPreview();
      return consent(true);
    }).catch(function () {
      videoStatus.textContent = "Camera or microphone permission was refused.";
    });
  }

  function stopSharing() {
    sharing = false;
    if (localStream) {
      localStream.getTracks().forEach(function (track) { track.stop(); });
      localStream = null;
    }
    Object.keys(peers).forEach(function (seat) { closePeer(Number(seat)); });
    videoToggle.textContent = "Turn on camera & mic";
    muteToggle.hidden = true;
    cameraToggle.hidden = true;
    videoStatus.textContent = "Camera and microphone are off.";
    consent(false);
  }

  // Stopping the tracks on page close matters: a camera light left on after
  // somebody navigates away is exactly the sort of thing that destroys trust.
  window.addEventListener("pagehide", function () {
    if (localStream) { localStream.getTracks().forEach(function (t) { t.stop(); }); }
  });

  function consent(on) {
    var body = new URLSearchParams();
    body.set("csrf", csrf);
    body.set("consent", on ? "1" : "0");
    return fetch("/poker/tables/" + tableID + "/video", { method: "POST", body: body });
  }

  function attachLocalPreview() {
    var video = document.getElementById("video-" + mySeat);
    if (video && localStream) {
      video.srcObject = localStream;
      video.muted = true;
    }
  }

  function reconcileMesh(view) {
    attachLocalPreview();
    if (!sharing) { return; }

    var live = {};
    (view.video_peers || []).forEach(function (seat) {
      if (seat === mySeat) { return; }
      live[seat] = true;
      // Only one side offers, or both would negotiate at once. The lower seat
      // number takes the initiative; it is arbitrary but it has to be agreed.
      if (!peers[seat] && mySeat < seat) { offerTo(seat); }
    });
    Object.keys(peers).forEach(function (seat) {
      if (!live[Number(seat)]) { closePeer(Number(seat)); }
    });
  }

  function peerFor(seat) {
    if (peers[seat]) { return peers[seat]; }
    var peer = new RTCPeerConnection({ iceServers: iceServers });
    peers[seat] = peer;

    if (localStream) {
      localStream.getTracks().forEach(function (track) { peer.addTrack(track, localStream); });
    }
    peer.onicecandidate = function (event) {
      if (event.candidate) { signal(seat, "ice", JSON.stringify(event.candidate)); }
    };
    peer.ontrack = function (event) {
      var video = document.getElementById("video-" + seat);
      if (video) { video.srcObject = event.streams[0]; }
    };
    peer.onconnectionstatechange = function () {
      if (peer.connectionState === "failed" || peer.connectionState === "closed") {
        closePeer(seat);
      }
    };
    return peer;
  }

  function closePeer(seat) {
    var peer = peers[seat];
    if (!peer) { return; }
    try { peer.close(); } catch (err) { /* already gone */ }
    delete peers[seat];
    var video = document.getElementById("video-" + seat);
    if (video) { video.srcObject = null; }
  }

  function offerTo(seat) {
    var peer = peerFor(seat);
    peer.createOffer().then(function (offer) {
      return peer.setLocalDescription(offer);
    }).then(function () {
      signal(seat, "offer", JSON.stringify(peer.localDescription));
    }).catch(function () { closePeer(seat); });
  }

  function onSignal(message) {
    if (!sharing) { return; }
    var seat = message.from;
    if (message.kind === "bye") { closePeer(seat); return; }

    var peer = peerFor(seat);
    if (message.kind === "offer") {
      peer.setRemoteDescription(JSON.parse(message.payload)).then(function () {
        return peer.createAnswer();
      }).then(function (answer) {
        return peer.setLocalDescription(answer);
      }).then(function () {
        signal(seat, "answer", JSON.stringify(peer.localDescription));
      }).catch(function () { closePeer(seat); });
    } else if (message.kind === "answer") {
      peer.setRemoteDescription(JSON.parse(message.payload)).catch(function () { closePeer(seat); });
    } else if (message.kind === "ice") {
      peer.addIceCandidate(JSON.parse(message.payload)).catch(function () { /* stale candidate */ });
    }
  }

  function signal(seat, kind, payload) {
    var body = new URLSearchParams();
    body.set("csrf", csrf);
    body.set("to", String(seat));
    body.set("kind", kind);
    body.set("payload", payload);
    fetch("/poker/tables/" + tableID + "/signal", { method: "POST", body: body })
      .catch(function () { /* the stream will resync */ });
  }
})();
