export function createUpdater(api, onChange, refreshSession) {
  const button = document.getElementById("checkUpdate");
  const panel = document.getElementById("updatePanel");
  const status = document.getElementById("updateStatus");
  const versions = document.getElementById("updateVersions");
  const install = document.getElementById("installUpdate");
  let data,
    loggedIn = false,
    detectionActive = false,
    checking = false;
  let busy = false,
    timer,
    reconnectUntil = 0,
    previousVersion = "";

  function render() {
    panel.hidden = !loggedIn;
    button.hidden = !loggedIn;
    button.disabled = checking || busy;
    button.textContent = checking ? "检查中…" : busy ? "同步中…" : "检查更新";
    install.disabled =
      checking || busy || detectionActive || !data?.supported || !data?.available;
    install.hidden = !data?.available || !data?.supported;
    install.textContent = busy
      ? "正在同步…"
      : `一键更新 (${data?.latest || "新版"})`;
    if (data) {
      versions.textContent = data.latest
        ? `${data.current} → ${data.latest}`
        : `当前 ${data.current}`;
      document.getElementById("updateUnsupported").hidden = data.supported;
      document.getElementById("updateRestartHint").hidden =
        !data.supported || !data.available;
      if (detectionActive && data.available && !busy)
        status.textContent = "检测进行中，完成后可更新";
    }
  }

  function receive(next) {
    const wasBusy = busy;
    data = next;
    busy = ["queued", "running"].includes(data.state);
    status.textContent = data.message;
    if (!busy && data.state !== "failed" && data.latest)
      status.textContent = data.available
        ? "发现仓库新版本"
        : "已是最新正式版";
    else if (!busy && data.state === "idle" && !data.latest)
      status.textContent = "可检查同步 GitHub 最新正式版";
    status.classList.toggle("error", data.state === "failed");
    render();
    if (busy !== wasBusy) onChange();
  }

  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(poll, 2000);
  }

  async function poll() {
    if (!loggedIn) return;
    try {
      const next = await api("/api/update");
      if (busy && !["queued", "running"].includes(next.state)) {
        if (!(await refreshSession())) return;
      }
      receive(next);
      if (busy) schedule();
      else if (
        next.state === "done" &&
        previousVersion &&
        next.current !== previousVersion
      )
        location.reload();
    } catch (error) {
      if (
        Date.now() < reconnectUntil &&
        error.status !== 401 &&
        error.status !== 403
      ) {
        status.textContent = "正在切换新版本并重连…";
        schedule();
      } else {
        busy = false;
        status.textContent = `${error.message} 请重连后重试。`;
        status.classList.add("error");
        render();
        onChange();
      }
    }
  }

  button.addEventListener("click", async () => {
    checking = true;
    status.textContent = "正在检查 GitHub 最新版本…";
    status.classList.remove("error");
    render();
    try {
      if (!(await refreshSession())) return;
      receive(await api("/api/update/check", {}));
      if (busy) {
        reconnectUntil = Date.now() + 12 * 60 * 1000;
        schedule();
      }
    } catch (error) {
      status.textContent = error.message;
      status.classList.add("error");
    } finally {
      checking = false;
      render();
    }
  });

  install.addEventListener("click", async () => {
    if (install.disabled) return;
    checking = true;
    previousVersion = data.current;
    render();
    try {
      if (!(await refreshSession())) return;
      receive(await api("/api/update/install", {}));
      if (busy) {
        reconnectUntil = Date.now() + 12 * 60 * 1000;
        schedule();
      }
    } catch (error) {
      status.textContent = error.message;
      status.classList.add("error");
    } finally {
      checking = false;
      render();
    }
  });

  return {
    get busy() {
      return busy;
    },
    setContext(session, active) {
      const detectionFinished = detectionActive && !active;
      loggedIn = Boolean(session.logged_in);
      detectionActive = active;
      if (!loggedIn) {
        clearTimeout(timer);
        panel.hidden = true;
        busy = false;
      }
      if (detectionFinished && data) receive(data);
      else render();
    },
    async restore() {
      receive(await api("/api/update"));
      if (busy) {
        previousVersion = data.current;
        reconnectUntil = Date.now() + 12 * 60 * 1000;
        render();
        schedule();
      }
    },
  };
}
