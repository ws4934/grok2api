// Page helpers for the muse.ai web app. Every CDP call evaluates
// (<this file>).<name>(...args), so nothing lives in the page between calls
// except buffers parked on window under a random key (putChunk, grab).
({
  bubbles() { return [...document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]')]; },
  isUser(b) { return /chat-user-bubble/.test(b.className || ''); },
  isAgent(b) { return /hatch-agent-bubble-bg/.test(b.className || ''); },
  lastUser() { const u = this.bubbles().filter((b) => this.isUser(b)); return u[u.length - 1] || null; },
  userCount() { return this.bubbles().filter((b) => this.isUser(b)).length; },
  // after reports whether node comes after ref in document order (true without ref).
  after(node, ref) { return !ref || !!(ref.compareDocumentPosition(node) & Node.DOCUMENT_POSITION_FOLLOWING); },
  visible(e) { return !!e && e.getClientRects().length > 0; },
  center(e) {
    if (!this.visible(e)) return null;
    const r = e.getBoundingClientRect();
    return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
  },

  // state is ready, login, error or loading.
  state() {
    if (location.protocol === 'chrome-error:') return 'error';
    const body = document.body ? document.body.innerText || '' : '';
    const ta = document.querySelector('textarea');
    const nav = performance.getEntriesByType('navigation')[0];
    if (!ta && (location.hostname !== 'muse.ai' || !location.pathname.startsWith('/thread') || (nav && nav.responseStatus === 401))
      && /\blog ?in\b|\bsign ?in\b|登录|登入|ログイン/i.test(body)) return 'login';
    if (document.readyState !== 'complete' || !ta || ta.disabled || location.hostname !== 'muse.ai') return 'loading';
    const h = document.querySelector('[data-hatch-shell-hydration-state]');
    if (h && h.getAttribute('data-hatch-shell-hydration-state') !== 'hydrated') return 'loading';
    if (body.includes('Connecting...')) return 'loading';
    return 'ready';
  },

  // text joins the page's text nodes outside the side chat list and outside
  // chat bubbles (only agent bubbles when withUser), so replies and prompts
  // that talk about limits never look like a limit banner.
  text(withUser) {
    const skip = 'script,style,template,noscript,textarea,[data-testid="hatch-side-chats-panel-shell"]';
    const self = this;
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT, {
      acceptNode(n) {
        if (n.nodeType === 3) return NodeFilter.FILTER_ACCEPT;
        if (n.matches(skip)) return NodeFilter.FILTER_REJECT;
        if (/hatch-chat-groupable-bubble/.test(n.className || '') && !(withUser && self.isUser(n))) return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_SKIP;
      },
    });
    let out = '';
    while (w.nextNode()) out += w.currentNode.nodeValue + ' ';
    return out;
  },

  // chat reports the Markdown of the agent bubbles after the latest user
  // bubble plus the composer flags.
  chat() {
    const ref = this.lastUser();
    const md = this.bubbles().filter((b) => this.isAgent(b) && this.after(b, ref)).map((b) => {
      const prose = [...b.querySelectorAll('[data-hatch-markdown-streaming]')]
        .filter((p) => !p.parentElement.closest('[data-hatch-markdown-streaming]'));
      return (prose.length ? prose : [b]).map((p) => this.md(p)).filter(Boolean).join('\n\n');
    }).filter(Boolean).join('\n\n');
    return {
      md,
      path: location.pathname,
      stop: !!document.querySelector('[data-testid="hatch-composer-stop-button"],button[aria-label*="Stop" i],button[aria-label*="停止"]'),
      stuck: /Still sending|Delivery not confirmed|正在发送|未确认送达/i.test(this.text(true)),
      // Specific phrases only: a "you used 80% of your weekly limit" notice
      // must not take a working account out for the week.
      limit: /limit reached|(?:reached|hit|exceeded|used (?:up |all )?(?:of )?) ?(?:your|the) (?:\w+ ){0,2}(?:limit|quota)|out of (?:credits|tokens|usage)|额度不足|积分不足|达到上限|已达上限|额度已用完/i.test(this.text(false)),
    };
  },

  // md converts rendered streamdown HTML back to Markdown.
  md(root) {
    const BLOCK = /^(P|DIV|SECTION|ARTICLE|HEADER|FOOTER|MAIN|ASIDE|NAV|FIGURE|FIGCAPTION|DETAILS|SUMMARY|H[1-6]|UL|OL|LI|PRE|BLOCKQUOTE|TABLE|THEAD|TBODY|TFOOT|TR|HR|DL|DT|DD)$/;
    const SKIP = 'button,svg,script,style,template,noscript,input:not([type=checkbox]),.sr-only,[data-copy-exclude="true"],'
      + '[data-streamdown="code-block-header"],[data-streamdown="code-block-actions"],.katex-mathml';
    const fence = (s, min) => {
      let n = min;
      for (const m of s.match(/`+/g) || []) n = Math.max(n, m.length + 1);
      return '`'.repeat(n);
    };
    const wrap = (s, mark) => {
      const m = /^(\s*)([\s\S]*?)(\s*)$/.exec(s);
      return m[1] + mark + m[2] + mark + m[3];
    };
    const inline = (nodes) => {
      let s = '';
      for (const c of nodes) {
        if (c.nodeType === 3) { s += c.nodeValue.replace(/[ \t\n\r\f]+/g, ' '); continue; }
        if (c.nodeType !== 1 || c.matches(SKIP)) continue;
        const tag = c.tagName;
        const ds = c.getAttribute('data-streamdown') || '';
        if (tag === 'BR') { s += '\n'; continue; }
        if (tag === 'INPUT') { s += c.checked ? '[x] ' : '[ ] '; continue; }
        if (tag === 'IMG') {
          const src = c.getAttribute('src') || '';
          if (src && !src.startsWith('blob:')) s += '![' + (c.getAttribute('alt') || '') + '](' + src + ')';
          continue;
        }
        if (c.classList.contains('katex')) {
          const tex = c.querySelector('annotation[encoding="application/x-tex"]');
          const mark = c.closest('.katex-display') ? '$$' : '$';
          s += tex ? mark + tex.textContent + mark : c.textContent;
          continue;
        }
        if (tag === 'CODE') {
          const t = c.textContent;
          const f = fence(t, 1);
          const pad = /^`|`$/.test(t) ? ' ' : '';
          s += f + pad + t + pad + f;
          continue;
        }
        const inner = inline(c.childNodes);
        if (!inner.trim()) { s += inner; continue; }
        if (tag === 'STRONG' || tag === 'B' || ds === 'strong') s += wrap(inner, '**');
        else if (tag === 'EM' || tag === 'I' || ds === 'emphasis') s += wrap(inner, '*');
        else if (tag === 'DEL' || tag === 'S' || ds === 'strikethrough') s += wrap(inner, '~~');
        else if (tag === 'A' && c.getAttribute('href')) s += '[' + inner.trim() + '](' + c.getAttribute('href') + ')';
        else s += inner;
      }
      return s;
    };
    const isBlock = (c) => c.nodeType === 1 && (BLOCK.test(c.tagName) || c.getAttribute('data-streamdown') === 'code-block');
    // block renders a container: inline runs become paragraphs, block
    // children render on their own, all separated by blank lines.
    const block = (el) => {
      const parts = [];
      let run = [];
      const flush = () => {
        const t = inline(run).replace(/ *\n */g, '\n').trim();
        if (t) parts.push(t);
        run = [];
      };
      for (const c of el.childNodes) {
        if (c.nodeType === 1 && c.matches(SKIP)) continue;
        if (isBlock(c)) {
          flush();
          const t = render(c);
          if (t) parts.push(t);
        } else run.push(c);
      }
      flush();
      return parts.join(el.tagName === 'LI' ? '\n' : '\n\n');
    };
    const render = (el) => {
      const tag = el.tagName;
      if (tag === 'PRE' || el.getAttribute('data-streamdown') === 'code-block') {
        const pre = tag === 'PRE' ? el : el.querySelector('pre');
        if (!pre) return '';
        const code = pre.querySelector('code') || pre;
        const lines = code.querySelectorAll(':scope > span.block');
        const text = (lines.length ? [...lines].map((l) => l.textContent).join('\n') : code.textContent).replace(/\n$/, '');
        const holder = el.closest('[data-language]');
        const lang = (holder && holder.getAttribute('data-language')) || ((code.className || '').match(/language-(\S+)/) || [])[1] || '';
        const f = fence(text, 3);
        return f + lang + '\n' + text + '\n' + f;
      }
      const h = /^H([1-6])$/.exec(tag);
      if (h) return '#'.repeat(Number(h[1])) + ' ' + inline(el.childNodes).replace(/\s+/g, ' ').trim();
      if (tag === 'UL' || tag === 'OL') {
        let n = parseInt(el.getAttribute('start') || '1', 10) || 1;
        const items = [];
        for (const li of el.children) {
          if (li.tagName !== 'LI') continue;
          const marker = tag === 'OL' ? (n++) + '. ' : '- ';
          items.push(marker + block(li).replace(/\n(?=.)/g, '\n' + ' '.repeat(marker.length)));
        }
        return items.join('\n');
      }
      if (tag === 'BLOCKQUOTE') return block(el).split('\n').map((l) => (l ? '> ' + l : '>')).join('\n');
      if (tag === 'TABLE') {
        const rows = [...el.querySelectorAll('tr')].map((tr) => [...tr.children]
          .map((td) => inline(td.childNodes).replace(/\s+/g, ' ').trim().replace(/\|/g, '\\|')));
        if (!rows.length) return '';
        const width = Math.max(...rows.map((r) => r.length));
        const line = (r) => '| ' + Array.from({ length: width }, (_, i) => r[i] || '').join(' | ') + ' |';
        return [line(rows[0]), line(Array(width).fill('---')), ...rows.slice(1).map(line)].join('\n');
      }
      if (tag === 'HR') return '---';
      return block(el);
    };
    return render(root).trim();
  },

  setPrompt(text) {
    const ta = document.querySelector('textarea');
    if (!ta) return false;
    ta.focus();
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(ta, text);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    return ta.value === text;
  },
  // clickSend clicks Send once React has enabled it.
  clickSend() {
    const b = [...document.querySelectorAll('button[aria-label]')]
      .find((x) => /^(send|发送|傳送|送出)$/i.test(x.getAttribute('aria-label').trim()));
    if (!b || b.disabled) return false;
    b.click();
    return true;
  },
  sent(users) {
    const ta = document.querySelector('textarea');
    return this.userCount() > users || (!!ta && ta.value === '');
  },

  removeButtons() {
    return [...document.querySelectorAll('button[aria-label]')]
      .filter((b) => /remove\s*attachment|移除附件|删除附件/i.test(b.getAttribute('aria-label')));
  },
  clearAttachments() { const b = this.removeButtons(); b.forEach((x) => x.click()); return b.length; },
  // attachmentCount is -1 while a file is still processing (its preview sits
  // in a role=status wrapper and Send stays disabled).
  attachmentCount() { return document.querySelector('[role=status] :is(img,video)') ? -1 : this.removeButtons().length; },
  putChunk(key, b64) {
    const bin = atob(b64);
    const u = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
    (window[key] = window[key] || []).push(u);
    return true;
  },
  // attach puts the parked files into the composer's file input.
  attach(files) {
    const input = document.querySelector('input[type=file]');
    if (!input) return 'no file input';
    const dt = new DataTransfer();
    for (const f of files) {
      dt.items.add(new File(window[f.key] || [], f.name, { type: f.type }));
      delete window[f.key];
    }
    input.files = dt.files;
    input.dispatchEvent(new Event('change', { bubbles: true }));
    return '';
  },

  // sources lists every result attachment source already on the page.
  sources() {
    return [...document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"] :is(img,video)')]
      .map((m) => m.currentSrc || m.src).filter(Boolean);
  },
  // media finds the newest loaded result attachment of kind after the latest
  // user bubble, skipping baseline sources, plus the chat state.
  media(kind, baseline) {
    const ref = this.lastUser();
    const all = [...document.querySelectorAll('[data-testid="hatch-chat-attachment-presentation-' + kind + '"]')]
      .filter((a) => !a.closest('[class*="chat-user-bubble"]') && this.after(a, ref));
    // Videos mount their <video> only when scrolled into view.
    if (all.length) all[all.length - 1].scrollIntoView({ block: 'center' });
    let ready = null;
    let pending = 0;
    for (const a of all) {
      let src = '';
      let w = 0;
      let h = 0;
      if (kind === 'image') {
        const i = a.querySelector('img');
        if (i && i.complete && i.naturalWidth > 0) { src = i.currentSrc || i.src; w = i.naturalWidth; h = i.naturalHeight; }
      } else {
        // Never wait for metadata: the headless shell cannot decode H.264.
        const v = a.querySelector('video');
        if (v) { src = v.currentSrc || v.src || (v.querySelector('source[src]') || {}).src || ''; w = v.videoWidth; h = v.videoHeight; }
      }
      if (src && baseline.includes(src)) continue;
      if (src) ready = { src, w, h };
      else pending++;
    }
    return Object.assign(this.chat(), { ready, pending });
  },
  async grab(src, key) {
    const r = await fetch(src);
    if (!r.ok) return { error: 'HTTP ' + r.status };
    const b = await r.blob();
    window[key] = new Uint8Array(await b.arrayBuffer());
    return { size: b.size, type: b.type };
  },
  chunk(key, off, n) {
    const a = window[key].subarray(off, off + n);
    let s = '';
    for (let i = 0; i < a.length; i += 32768) s += String.fromCharCode.apply(null, a.subarray(i, i + 32768));
    return btoa(s);
  },
  drop(key) { delete window[key]; return true; },

  dockMore() { return this.center(document.querySelector('[data-testid="hatch-dock-more"]')); },
  menuOpen() { return [...document.querySelectorAll('[role=menu]')].some((e) => this.visible(e)); },
  settingsItem() {
    return this.center([...document.querySelectorAll('[role=menuitem]')]
      .find((e) => this.visible(e) && /^(Settings|設定|设置)/.test((e.textContent || '').trim())));
  },
  dialogText() {
    const d = [...document.querySelectorAll('[role=dialog],[aria-modal=true]')].find((e) => this.visible(e));
    return d ? d.innerText || '' : '';
  },
})
