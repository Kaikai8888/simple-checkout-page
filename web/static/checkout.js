// Checkout page.
//   /checkout.html?items=1:2,2:1        (product_id:quantity, comma separated)
//   /checkout.html?product_id=1&qty=1   (single product, still supported)
//   &draft=<draft_id>                   (added after submit: a reload resumes polling instead of re-submitting)
const $ = (id) => document.getElementById(id);
const MAX_QTY = 10;
const MAX_ADDON_QTY = 5;

const state = {
  options: null,
  catalog: {},         // product_id -> product (latest from API)
  cart: [],            // [{ id, qty }] main items, in display order
  addons: [],          // add-on products not already in the cart
  addonQty: {},        // product_id -> qty (0 = not selected)
  missing: [],         // ids in the URL that no longer exist
  coupon: null,        // { code, discount, description }
  submitting: false,
  idem: null,          // { key, body } reused only when re-sending the exact same payload (network retry)
  draftId: null,       // submitted checkout we are waiting for; kept in the URL so a reload never re-submits
};

init();

async function init() {
  state.cart = parseCartFromUrl();
  try {
    state.options = await api.get('/api/checkout/options');
    await loadProducts();
  } catch (err) {
    $('load-error').textContent = `無法載入商品：${err.message}`;
    $('load-error').classList.remove('hidden');
    return;
  }
  clampQuantities();

  renderPaymentMethods();
  renderRegions();
  renderItems();
  renderAddons();
  bindEvents();
  recalc();
  $('checkout').classList.remove('hidden');

  const draftId = new URLSearchParams(location.search).get('draft');
  if (draftId) {
    state.draftId = draftId;
    waitForDraft({ resumed: true });
  }
}

function parseCartFromUrl() {
  const params = new URLSearchParams(location.search);
  const merged = new Map();
  const add = (id, qty) => {
    if (Number.isInteger(id) && id > 0 && Number.isInteger(qty) && qty > 0) merged.set(id, (merged.get(id) || 0) + qty);
  };
  if (params.get('items')) {
    for (const part of params.get('items').split(',')) {
      const [id, qty] = part.split(':').map(Number);
      add(id, qty || 1);
    }
  } else if (params.get('product_id')) {
    add(Number(params.get('product_id')), Number(params.get('qty') || 1));
  }
  return [...merged].map(([id, qty]) => ({ id, qty }));
}

function syncUrl() {
  const items = state.cart.map((c) => `${c.id}:${c.qty}`).join(',');
  const query = [items && `items=${items}`, state.draftId && `draft=${encodeURIComponent(state.draftId)}`].filter(Boolean).join('&');
  history.replaceState(null, '', query ? `?${query}` : location.pathname);
  try { localStorage.setItem('cart', JSON.stringify(Object.fromEntries(state.cart.map((c) => [c.id, c.qty])))); } catch (_) { /* optional */ }
}

async function loadProducts() {
  const { products } = await api.get('/api/products');
  state.catalog = Object.fromEntries(products.map((p) => [p.id, p]));
  state.missing = state.cart.filter((c) => !state.catalog[c.id]).map((c) => c.id);
  state.cart = state.cart.filter((c) => state.catalog[c.id]);
  const inCart = new Set(state.cart.map((c) => c.id));
  state.addons = products.filter((p) => p.is_addon && !inCart.has(p.id));
}

// ------------------------------------------------------------------ rendering

function renderItems() {
  if (state.cart.length === 0) {
    $('items').innerHTML = `<div class="empty">購物車是空的<br><a href="/">前往選購商品</a></div>`;
    updateSubmitState();
    return;
  }
  const missing = state.missing.length ? `<div class="muted danger">商品 #${state.missing.join(', #')} 已下架，已從清單移除</div>` : '';
  $('items').innerHTML = missing + state.cart.map(({ id, qty }) => {
    const p = state.catalog[id];
    const max = Math.min(p.available_stock, MAX_QTY);
    const badge = discountLabel(p.price, p.original_price);
    const opts = max > 0
      ? Array.from({ length: max }, (_, i) => `<option value="${i + 1}" ${qty === i + 1 ? 'selected' : ''}>${i + 1}</option>`).join('')
      : '<option value="0">售完</option>';
    const details = p.description || (p.highlights || []).length
      ? `<details class="more"><summary>顯示品項細節</summary>
           ${p.description ? `<div class="promo">${escapeHtml(p.description)}</div>` : ''}
           <ul class="highlights">${(p.highlights || []).map((h) => `<li>${escapeHtml(h)}</li>`).join('')}</ul>
         </details>`
      : '';
    return `
      <div class="item ${p.available_stock <= 0 ? 'soldout' : ''}">
        <div class="cover thumb ${p.id === 1 ? '' : 'alt'}">${escapeHtml(p.name.slice(0, 8))}</div>
        <div class="item-body">
          <div class="item-name">${escapeHtml(p.name)}</div>
          <div class="price-row"><span class="price money">${money(p.price)}</span>${badge ? `<span class="badge">${badge}</span>` : ''}</div>
          ${p.original_price > p.price ? `<div class="orig">預定售價 <s>${money(p.original_price)}</s></div>` : ''}
          <div class="stock-row"><span class="stock-tag ${p.available_stock > 20 ? 'ok' : ''}">${p.available_stock > 0 ? `剩餘 ${p.available_stock} 份` : '已售完'}</span>${p.flash_sale ? '<span class="flash-tag">⚡ 限量搶購</span>' : ''}</div>
          <div class="qty">數量 <select data-item-qty="${id}" ${max <= 0 ? 'disabled' : ''}>${opts}</select>
            <button type="button" class="link-btn" data-remove="${id}">移除</button></div>
          ${details}
        </div>
      </div>`;
  }).join('');
  updateSubmitState();
}

function renderAddons() {
  if (state.addons.length === 0) {
    $('addons').innerHTML = '<div class="muted">沒有其他加購選項</div>';
    return;
  }
  $('addons').innerHTML = state.addons.map((a) => {
    const qty = state.addonQty[a.id] || 0;
    const max = Math.min(a.available_stock, MAX_ADDON_QTY);
    const opts = Array.from({ length: Math.max(max, 1) }, (_, i) => `<option value="${i + 1}" ${qty === i + 1 ? 'selected' : ''}>${i + 1}</option>`).join('');
    return `
      <div class="addon">
        <input type="checkbox" data-addon="${a.id}" ${qty > 0 ? 'checked' : ''} ${a.available_stock <= 0 ? 'disabled' : ''}>
        <div class="name">${escapeHtml(a.name)}<div class="muted">${money(a.price)}・${a.available_stock > 0 ? `剩 ${a.available_stock} 件` : '已售完'}</div></div>
        <select data-addon-qty="${a.id}" ${qty > 0 ? '' : 'disabled'}>${opts}</select>
      </div>`;
  }).join('');
}

function renderPaymentMethods() {
  $('payment-methods').innerHTML = state.options.payment_methods.map((m, i) => `
    <label class="option ${i === 0 ? 'selected' : ''}">
      <input type="radio" name="payment" value="${m.code}" ${i === 0 ? 'checked' : ''}>
      <div><b>${escapeHtml(m.name)}</b><small>${escapeHtml(m.description)}</small></div>
    </label>`).join('');
}

function renderRegions() {
  $('region').innerHTML = state.options.shipping_regions
    .map((r) => `<option value="${r.code}">${escapeHtml(r.name)}${r.fee ? `（運費 ${money(r.fee)}）` : '（免運）'}</option>`).join('');
  $('region').value = 'TW_MAIN';
  onRegionChange('新北市', '新莊區');
}

function onRegionChange(defaultCounty, defaultDistrict) {
  const region = currentRegion();
  const counties = TW_ADDRESS[region.code];
  $('tw-fields').classList.toggle('hidden', !region.domestic);
  $('zip').readOnly = !!region.domestic;
  if (!region.domestic) {
    $('zip').value = '';
    $('zip').placeholder = 'Postal code';
    $('address').placeholder = 'Full address (street, city, state)';
    return;
  }
  $('address').placeholder = '路名、門牌、樓層';
  $('county').innerHTML = Object.keys(counties).map((c) => `<option>${c}</option>`).join('');
  if (defaultCounty && counties[defaultCounty]) $('county').value = defaultCounty;
  onCountyChange(defaultDistrict);
}

function onCountyChange(defaultDistrict) {
  const districts = TW_ADDRESS[currentRegion().code][$('county').value] || {};
  $('district').innerHTML = Object.keys(districts).map((d) => `<option>${d}</option>`).join('');
  if (defaultDistrict && districts[defaultDistrict]) $('district').value = defaultDistrict;
  $('zip').value = districts[$('district').value] || '';
}

// ------------------------------------------------------------------ pricing (display only; backend recomputes)

function mainItems() {
  return state.cart
    .filter((c) => c.qty > 0 && state.catalog[c.id]?.available_stock > 0)
    .map((c) => ({ product_id: c.id, quantity: c.qty, unit_price: state.catalog[c.id].price }));
}

function addonItems() {
  return state.addons
    .filter((a) => (state.addonQty[a.id] || 0) > 0)
    .map((a) => ({ product_id: a.id, quantity: state.addonQty[a.id], unit_price: a.price }));
}

function selectedItems() {
  return [...mainItems(), ...addonItems()];
}

const sum = (items) => items.reduce((s, it) => s + it.unit_price * it.quantity, 0);
const subtotal = () => sum(selectedItems());

function currentRegion() {
  return state.options.shipping_regions.find((r) => r.code === $('region').value);
}

function donation() {
  const v = Math.floor(Number($('donation').value));
  return Number.isFinite(v) && v > 0 ? v : 0;
}

function computeTotal() {
  const main = sum(mainItems());
  const addon = sum(addonItems());
  const ship = currentRegion().fee;
  const disc = state.coupon ? state.coupon.discount : 0;
  return { main, addon, ship, donation: donation(), discount: disc, total: main + addon - disc + ship + donation() };
}

function recalc() {
  const t = computeTotal();
  $('s-main').textContent = money(t.main);
  $('s-addon').textContent = '+ ' + money(t.addon);
  $('s-ship-label').textContent = `運費：${currentRegion().name.split(' ')[0]}`;
  $('s-ship').textContent = '+ ' + money(t.ship);
  $('s-donation-row').classList.toggle('hidden', t.donation === 0);
  $('s-donation').textContent = '+ ' + money(t.donation);
  $('s-discount-row').classList.toggle('hidden', t.discount === 0);
  $('s-discount').textContent = '- ' + money(t.discount);
  $('s-total').textContent = money(t.total);
  $('submit-total').textContent = money(t.total);
}

let couponTimer;
function onSubtotalChanged() {
  recalc();
  if (!state.coupon) return;
  clearTimeout(couponTimer);
  couponTimer = setTimeout(() => applyCoupon(state.coupon.code, true), 250);
}

async function applyCoupon(code, silent = false) {
  code = (code || '').trim().toUpperCase();
  const msg = $('coupon-msg');
  if (!code) {
    state.coupon = null;
    msg.textContent = '';
    recalc();
    return;
  }
  try {
    const res = await api.post('/api/coupons/preview', { code, subtotal: subtotal() });
    state.coupon = res;
    $('coupon-input').value = res.code;
    msg.className = 'coupon-msg muted';
    msg.textContent = `✔ ${res.description}（折抵 ${money(res.discount)}）`;
  } catch (err) {
    state.coupon = null;
    msg.className = 'coupon-msg danger';
    msg.textContent = silent ? `折扣碼已失效：${err.message}` : err.message;
  }
  recalc();
}

// ------------------------------------------------------------------ events

function bindEvents() {
  $('items').addEventListener('change', (e) => {
    const id = Number(e.target.dataset.itemQty);
    if (!id) return;
    state.cart.find((c) => c.id === id).qty = Number(e.target.value);
    syncUrl();
    onSubtotalChanged();
  });
  $('items').addEventListener('click', (e) => {
    const id = Number(e.target.dataset.remove);
    if (!id) return;
    state.cart = state.cart.filter((c) => c.id !== id);
    if (state.catalog[id]?.is_addon) state.addons.push(state.catalog[id]); // removed add-on becomes selectable again
    syncUrl();
    renderItems();
    renderAddons();
    onSubtotalChanged();
  });
  $('addons').addEventListener('change', (e) => {
    const t = e.target;
    if (t.dataset.addon) {
      const id = Number(t.dataset.addon);
      const sel = document.querySelector(`[data-addon-qty="${id}"]`);
      sel.disabled = !t.checked;
      state.addonQty[id] = t.checked ? Number(sel.value) : 0;
    } else if (t.dataset.addonQty) {
      state.addonQty[Number(t.dataset.addonQty)] = Number(t.value);
    }
    onSubtotalChanged();
  });
  $('payment-methods').addEventListener('change', () => {
    document.querySelectorAll('#payment-methods .option').forEach((el) => el.classList.toggle('selected', el.querySelector('input').checked));
  });
  $('donation').addEventListener('input', recalc);
  $('region').addEventListener('change', () => { onRegionChange(); recalc(); });
  $('county').addEventListener('change', () => onCountyChange());
  $('district').addEventListener('change', () => {
    $('zip').value = (TW_ADDRESS[currentRegion().code][$('county').value] || {})[$('district').value] || '';
  });
  $('coupon-apply').addEventListener('click', () => applyCoupon($('coupon-input').value));
  $('coupon-input').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); applyCoupon(e.target.value); } });
  document.querySelectorAll('input[name=invoice-type]').forEach((el) => el.addEventListener('change', () => {
    const company = document.querySelector('input[name=invoice-type]:checked').value === 'company';
    $('invoice-company').classList.toggle('hidden', !company);
    $('invoice-personal').classList.toggle('hidden', company);
  }));
  $('changed-ok').addEventListener('click', () => $('changed').classList.add('hidden'));
  $('overlay-retry').addEventListener('click', () => waitForDraft({ resumed: false }));
  $('form').addEventListener('submit', submit);
}

// ------------------------------------------------------------------ submit flow

function buildPayload() {
  const region = currentRegion();
  const invoiceType = document.querySelector('input[name=invoice-type]:checked').value;
  const name = $('recipient-name').value.trim();
  const phone = $('recipient-phone').value.trim();
  return {
    items: selectedItems(),
    coupon_code: state.coupon ? state.coupon.code : '',
    donation: donation(),
    payment_method: document.querySelector('input[name=payment]:checked').value,
    shipping: {
      region: region.code,
      county: region.domestic ? $('county').value : '',
      district: region.domestic ? $('district').value : '',
      address: $('address').value.trim(),
      zip: $('zip').value.trim(),
      recipient_name: name,
      recipient_phone: phone,
    },
    customer: { name, email: $('email').value.trim(), phone },
    invoice: invoiceType === 'company'
      ? { type: 'company', tax_id: $('tax-id').value.trim(), title: $('invoice-title').value.trim() }
      : { type: 'personal', carrier: $('carrier').value.trim().toUpperCase() },
    note: $('note').value.trim(),
    expected_total: computeTotal().total, // what the shopper confirmed; backend rejects if it differs
  };
}

function validate(p) {
  const errs = [];
  const domestic = currentRegion().domestic;
  if (mainItems().length === 0) errs.push('請至少選擇一項可購買的商品');
  if (!p.shipping.address) errs.push('請填寫地址');
  if (!p.shipping.recipient_name) errs.push('請填寫收件人');
  if (domestic && !/^09\d{8}$/.test(p.shipping.recipient_phone)) errs.push('請填寫正確的手機號碼（09xxxxxxxx）');
  if (!domestic && !p.shipping.recipient_phone) errs.push('請填寫聯絡電話');
  if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(p.customer.email)) errs.push('請填寫正確的 Email');
  if (p.invoice.type === 'personal' && p.invoice.carrier && !/^\/[0-9A-Z.+-]{7}$/.test(p.invoice.carrier)) errs.push('手機載具格式錯誤（例：/AB1CD23）');
  if (p.invoice.type === 'company' && !/^\d{8}$/.test(p.invoice.tax_id)) errs.push('統一編號需為 8 碼數字');
  if (p.invoice.type === 'company' && !p.invoice.title) errs.push('請填寫發票抬頭');
  return errs;
}

async function submit(e) {
  e.preventDefault();
  if (state.submitting) return;
  hideErrors();
  const payload = buildPayload();
  const errs = validate(payload);
  if (errs.length) return showErrors(errs);

  // Same body => same key (safe retry after a network error). Any change => new attempt.
  const body = JSON.stringify(payload);
  if (!state.idem || state.idem.body !== body) state.idem = { key: newKey(), body };

  setSubmitting(true);
  showOverlay('送出訂單中…');
  let draft;
  try {
    draft = await api.post('/api/checkouts', payload, { 'Idempotency-Key': state.idem.key });
  } catch (err) {
    hideOverlay();
    setSubmitting(false);
    if (!err.status) return showErrors(['網路連線異常，請再按一次送出（不會重複下單）']);
    state.idem = null;
    return handleCheckoutError(err);
  }
  state.idem = null;
  state.draftId = draft.draft_id;
  syncUrl(); // from now on a reload resumes this draft instead of submitting a new one
  return waitForDraft({ resumed: false });
}

// Polls the submitted draft until it is completed / failed. On timeout the shopper is NOT allowed to submit
// again (that could create a second order); they can only keep waiting for the same draft.
async function waitForDraft({ resumed }) {
  setSubmitting(true);
  showOverlay(resumed ? '正在查詢您剛送出的訂單…' : '訂單處理中，請勿關閉視窗…');
  const result = await pollDraft(state.draftId);

  if (result.status === 'completed') {
    // Fresh submit: go straight to payment. Resumed (reload / back button): show the order page,
    // which offers "pay" or "retry payment" without silently opening a new payment session.
    if (resumed) {
      showOverlay('訂單已成立，正在前往訂單頁…');
      location.href = `/result.html?order_id=${encodeURIComponent(result.order_id)}`;
      return;
    }
    return goPay(result.order_id);
  }
  if (result.status === 'timeout') return showStillProcessing();

  clearDraft();
  hideOverlay();
  setSubmitting(false);
  if (result.status === 'failed') return handleCheckoutError(result.error);
  showErrors(['找不到這筆結帳紀錄，請確認購物內容後重新送出。']);
}

function showStillProcessing() {
  $('overlay-spinner').classList.add('hidden');
  $('overlay-text').textContent = '訂單仍在處理中';
  $('overlay-sub').textContent = '系統目前較忙碌。您的訂單已送出，請勿重新下單；重新整理或稍後回到此頁面，也會繼續查詢同一筆訂單。';
  $('overlay-actions').classList.remove('hidden');
}

function clearDraft() {
  state.draftId = null;
  syncUrl();
}

async function pollDraft(draftId) {
  const started = Date.now();
  for (let i = 0; Date.now() - started < 60_000; i++) {
    await sleep(i < 5 ? 300 : 1000);
    try {
      const d = await api.get(`/api/checkouts/${encodeURIComponent(draftId)}`);
      if (d.status === 'completed' || d.status === 'failed') return d;
    } catch (err) {
      if (err.status === 404) return { status: 'not_found' };
      /* transient (network / 5xx): keep polling */
    }
    $('overlay-sub').textContent = `已等待 ${Math.round((Date.now() - started) / 1000)} 秒`;
  }
  return { status: 'timeout' };
}

async function goPay(orderId) {
  try { localStorage.removeItem('cart'); } catch (_) { /* optional */ }
  showOverlay('訂單已成立，正在前往付款頁…');
  try {
    const { payment_url } = await api.post(`/api/orders/${orderId}/payments`);
    location.href = payment_url;
  } catch (_) {
    location.href = `/result.html?order_id=${encodeURIComponent(orderId)}`;
  }
}

async function handleCheckoutError(err) {
  const code = err.code;
  if (code === 'ITEMS_CHANGED') return refreshAfterChange(err.details?.items || []);
  if (code === 'AMOUNT_MISMATCH') return refreshAfterChange([], '訂單金額已有變動（例如運費或折扣規則），已為您重新計算，請確認後再送出。');
  if (code === 'COUPON_INVALID') {
    state.coupon = null;
    $('coupon-msg').className = 'coupon-msg danger';
    $('coupon-msg').textContent = err.message;
    recalc();
    return showErrors([`折扣碼無法使用：${err.message}，已移除折扣，請確認金額後再送出。`]);
  }
  if (code === 'INVALID_REQUEST' && Array.isArray(err.details)) return showErrors(err.details);
  showErrors([err.message || '下單失敗，請稍後再試']);
}

// Items changed: fetch the latest product info, update the page, and ask the shopper to confirm again.
async function refreshAfterChange(issues, desc) {
  try { await loadProducts(); } catch (_) { /* keep old data */ }
  clampQuantities();
  syncUrl();
  renderItems();
  renderAddons();
  if (state.coupon) await applyCoupon(state.coupon.code, true);
  recalc();

  const nameOf = (id) => state.catalog[id]?.name || `商品 #${id}`;
  const lines = issues.map((it) => {
    const name = escapeHtml(it.name || nameOf(it.product_id));
    switch (it.reason) {
      case 'PRICE_CHANGED': return `${name}：價格由 ${money(it.expected_price)} 調整為 <b>${money(it.current_price)}</b>`;
      case 'SOLD_OUT': return `${name}：<b>已售完</b>`;
      case 'INSUFFICIENT_STOCK': return `${name}：庫存僅剩 <b>${it.available_stock}</b> 件（您選擇 ${it.requested_qty} 件），已調整數量`;
      case 'NOT_FOUND': return `${name}：已下架`;
      default: return name;
    }
  });
  $('changed-desc').textContent = desc || '下單期間商品資訊有變動，已為您更新結帳頁，請確認後再送出。';
  $('changed-list').innerHTML = lines.map((l) => `<li>${l}</li>`).join('');
  $('changed').classList.remove('hidden');
}

function clampQuantities() {
  for (const c of state.cart) {
    const stock = state.catalog[c.id].available_stock;
    c.qty = stock > 0 ? Math.max(1, Math.min(c.qty, stock, MAX_QTY)) : 0;
  }
  for (const a of state.addons) {
    if (state.addonQty[a.id]) state.addonQty[a.id] = Math.min(state.addonQty[a.id], a.available_stock, MAX_ADDON_QTY);
  }
}

// ------------------------------------------------------------------ ui helpers

function newKey() {
  return crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}
function updateSubmitState() { $('submit').disabled = state.submitting || mainItems().length === 0; }
function setSubmitting(v) { state.submitting = v; updateSubmitState(); }
function showOverlay(text) {
  $('overlay-text').textContent = text;
  $('overlay-sub').textContent = '';
  $('overlay-spinner').classList.remove('hidden');
  $('overlay-actions').classList.add('hidden');
  $('overlay').classList.remove('hidden');
}
function hideOverlay() { $('overlay').classList.add('hidden'); }
function showErrors(list) {
  $('form-errors').innerHTML = `<b>請確認以下資訊</b><ul>${list.map((e) => `<li>${escapeHtml(e)}</li>`).join('')}</ul>`;
  $('form-errors').classList.remove('hidden');
  $('form-errors').scrollIntoView({ behavior: 'smooth', block: 'center' });
}
function hideErrors() { $('form-errors').classList.add('hidden'); }
