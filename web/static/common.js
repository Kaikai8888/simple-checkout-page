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
    if (res.status === 401 && !path.startsWith('/api/auth/') && !path.startsWith('/api/payments/')) {
      goToLogin(); // session missing or expired while using a protected API
    }
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

// ------------------------------------------------------------------ auth

// Returns the logged-in user, or null. The session lives in an HttpOnly cookie that JS cannot read.
async function currentUser() {
  try {
    const { user } = await api.get('/api/auth/me');
    return user;
  } catch (_) {
    return null;
  }
}

function goToLogin() {
  location.href = '/login.html?next=' + encodeURIComponent(location.pathname + location.search);
}

// Pages that need an account call this first; it redirects to the login page and returns null if logged out.
async function requireLogin() {
  const user = await currentUser();
  if (!user) goToLogin();
  else renderTopbarAuth(user);
  return user;
}

// Shows "email · 登出" (or a login link) in the top bar; the admin link is only visible to admins.
function renderTopbarAuth(user) {
  const bar = document.querySelector('.topbar');
  if (!bar) return;
  bar.querySelectorAll('.admin-link').forEach((a) => a.classList.toggle('hidden', !user || user.role !== 'admin'));
  let box = bar.querySelector('.auth-box');
  if (!box) {
    box = document.createElement('span');
    box.className = 'auth-box';
    bar.appendChild(box);
  }
  if (user) {
    box.innerHTML = `<span class="who">${escapeHtml(user.email)}</span><button type="button" class="link-btn" id="logout-btn">登出</button>`;
    box.querySelector('#logout-btn').onclick = async () => {
      await api.post('/api/auth/logout').catch(() => {});
      location.href = '/';
    };
  } else {
    box.innerHTML = `<a href="/login.html?next=${encodeURIComponent(location.pathname + location.search)}">登入 / 註冊</a>`;
  }
}
