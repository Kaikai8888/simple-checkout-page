// Shared helpers for all pages.
const api = {
  async request(method, path, body, headers = {}) {
    const res = await fetch(path, {
      method,
      headers: { 'Content-Type': 'application/json', ...headers },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = null;
    try { data = await res.json(); } catch (_) { /* empty body */ }
    if (!res.ok) {
      const err = new Error(data?.error?.message || `HTTP ${res.status}`);
      err.status = res.status;
      err.code = data?.error?.code;
      err.details = data?.error?.details;
      throw err;
    }
    return data;
  },
  get(path) { return this.request('GET', path); },
  post(path, body, headers) { return this.request('POST', path, body, headers); },
  put(path, body) { return this.request('PUT', path, body); },
};

function money(n) {
  return 'NT$ ' + Number(n || 0).toLocaleString('en-US');
}

function escapeHtml(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function discountLabel(price, original) {
  if (!original || original <= price) return '';
  const pct = Math.round((price / original) * 100);
  return `${pct % 10 === 0 ? pct / 10 : pct} 折`;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PAYMENT_NAMES = { credit_card: '信用卡付款', aftee: 'AFTEE', atm: 'ATM 轉帳' };
const ORDER_STATUS = {
  pending_payment: '等待付款',
  paid: '付款完成',
  expired: '已逾期取消',
  refund_required: '付款逾時，將退款',
};
