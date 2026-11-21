/* 后台唯一的脚本：把指定元素里的文本写进剪贴板。
 *
 * 为什么需要它：CSP 不允许内联脚本，剪贴板 API 又只能由脚本调用。
 * 这里刻意做得很窄——只按 data-copy 指向的元素 id 取内容，
 * 不拼 URL、不发请求、不读其它 DOM，所以即使文件被替换，
 * 它能碰到的也只有页面上本来就显示了的那两段文字。
 */
(function () {
  'use strict';

  function target(id) {
    return document.getElementById(id);
  }

  function textOf(el) {
    if (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT') return el.value;
    return el.textContent;
  }

  /* 老浏览器 / 非安全上下文没有 navigator.clipboard：退化成全选，
     用户长按即可复制。按钮上会写清楚"已选中"，不至于点完没反应。 */
  function selectAll(el) {
    if (el.select) {
      el.focus();
      el.select();
      return true;
    }
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    if (!sel) return false;
    sel.removeAllRanges();
    sel.addRange(range);
    return true;
  }

  function flashLabel(btn, text) {
    if (btn.dataset.busy === '1') return;
    var old = btn.textContent;
    btn.dataset.busy = '1';
    btn.textContent = text;
    window.setTimeout(function () {
      btn.textContent = old;
      btn.dataset.busy = '';
    }, 1800);
  }

  /* 提示词是固定的几段文字，撑开到实际高度比给个滚动条好：
     否则用户会以为内容被截断了，或者以为那就是全文。 */
  function autosize(el) {
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 2 + 'px';
  }

  function init() {
    var boxes = document.querySelectorAll('textarea.copybox');
    for (var i = 0; i < boxes.length; i++) autosize(boxes[i]);
    bindFolds();
  }

  /* 折叠里的 textarea 在 display:none 状态下 scrollHeight 是 0，
     按这个值撑高，展开后只剩两像素。所以在每次展开时重新量一次。 */
  function bindFolds() {
    var folds = document.querySelectorAll('details.fold');
    for (var i = 0; i < folds.length; i++) {
      folds[i].addEventListener('toggle', function () {
        if (!this.open) return;
        var ta = this.querySelector('textarea.copybox');
        if (ta) autosize(ta);
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    e.preventDefault();

    var el = target(btn.getAttribute('data-copy'));
    if (!el) return;
    var text = textOf(el);

    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () {
        flashLabel(btn, '已复制');
      }, function () {
        selectAll(el);
        flashLabel(btn, '已选中，请复制');
      });
      return;
    }
    selectAll(el);
    flashLabel(btn, '已选中，请复制');
  });
})();
