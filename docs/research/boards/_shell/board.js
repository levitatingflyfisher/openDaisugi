  const ITEMS = window.ITEMS || [];
  const board = document.getElementById('board');
  const empty = document.getElementById('boardEmpty');
  function esc(s){ return String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
  function host(u){ try{ return new URL(u).host.replace(/^www\./,''); }catch(e){ return u; } }
  board.innerHTML = ITEMS.map(it => `
    <figure class="card" data-group="${esc(it.group)}" data-verdict="${esc(it.verdict)}">
      <img src="${esc(it.file)}" alt="${esc(it.title)}: ${esc(it.caption)}" loading="lazy">
      <figcaption class="body">
        <div class="row"><h3>${esc(it.title)}</h3><span class="tag ${esc(it.verdict)}">${esc(it.verdict_label || it.verdict)}</span></div>
        <p>${esc(it.caption)}</p>
        ${it.note ? `<p><em>${esc(it.note)}</em></p>` : ''}
        <a class="src" href="${esc(it.source_page)}" target="_blank" rel="noopener">${esc(host(it.source_page))}</a>
      </figcaption>
    </figure>`).join('');
  const chips = document.querySelectorAll('.chip');
  chips.forEach(c => c.addEventListener('click', () => {
    chips.forEach(x => x.setAttribute('aria-pressed', x === c ? 'true' : 'false'));
    const f = c.dataset.f; let shown = 0;
    document.querySelectorAll('.card').forEach(card => {
      const ok = f === 'all' || card.dataset.group === f || card.dataset.verdict === f;
      card.hidden = !ok; if (ok) shown++;
    });
    empty.hidden = shown > 0;
  }));
  empty.hidden = ITEMS.length > 0;
