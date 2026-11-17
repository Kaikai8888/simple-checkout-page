// Result page: /result.html?order_id=...
// The browser is redirected here by the gateway. The payment result arrives separately via the
// server-to-server callback, so we poll the order until it leaves the "waiting for callback" state.
const $ = (id) => document.getElementById(id);
const orderId = new URLSearchParams(location.search).get('order_id');

main();

async function main() {
  if (!orderId) return showError('缺少訂單編號');
  const started = Date.now();
  while (true) {
    let order;
    try {
      order = await api.get(`/api/orders/${encodeURIComponent(orderId)}`);
    } catch (err) {
      return showError(err.message);
    }
    render(order);
    const waitingCallback = order.status === 'pending_payment' && order.latest_payment?.status === 'pending';
    if (!waitingCallback || Date.now() - started > 30_000) return;
    await sleep(1500);
  }
}

function render(o) {
  $('order').classList.remove('hidden');
  const pay = o.latest_payment;
  const st = $('status');
  st.className = `status ${o.status}`;
  st.textContent = ORDER_STATUS[o.status] || o.status;

  let headline = '訂單已成立';
  let hint = '';
  if (o.status === 'paid') headline = '付款成功，感謝您的支持！';
  else if (o.status === 'expired') headline = '訂單已逾期取消';
  else if (o.status === 'refund_required') { headline = '付款已收到，但訂單已逾期'; hint = '客服將主動與您聯繫退款事宜。'; }
  else if (pay?.status === 'failed') { headline = '付款未完成'; hint = `原因：${pay.failure_reason || '付款失敗'}。您可以在期限內重新付款。`; }
  else if (pay?.status === 'pending') { headline = '等待付款結果…'; hint = '若您已完成付款，結果可能需要數秒鐘更新。'; }

  $('headline').textContent = headline;
  $('subline').textContent = `訂單編號 ${o.order_no}・${PAYMENT_NAMES[o.payment_method] || o.payment_method}` +
    (o.status === 'pending_payment' ? `・請於 ${new Date(o.expires_at).toLocaleString()} 前完成付款` : '');
  $('payment-hint').textContent = hint;
  $('payment-hint').classList.toggle('hidden', !hint);

  $('actions').innerHTML = '';
  if (o.status === 'pending_payment') {
    const btn = document.createElement('button');
    btn.className = 'btn btn-primary';
    btn.style.marginTop = '0';
    btn.textContent = pay?.status === 'pending' ? '前往付款頁' : '重新付款';
    btn.onclick = async () => {
      btn.disabled = true;
      try {
        const { payment_url } = await api.post(`/api/orders/${encodeURIComponent(o.id)}/payments`);
        location.href = payment_url;
      } catch (err) {
        btn.disabled = false;
        showError(err.message);
      }
    };
    $('actions').appendChild(btn);
  }

  $('lines').innerHTML = o.lines.map((l) => `
    <tr><td>${escapeHtml(l.name)}</td><td class="money">${money(l.unit_price)}</td><td>${l.quantity}</td>
    <td class="money" style="text-align:right">${money(l.amount)}</td></tr>`).join('');

  const row = (label, value) => `<div class="line"><span>${label}</span><span class="money">${value}</span></div>`;
  $('totals').innerHTML =
    row('商品小計', money(o.subtotal)) +
    (o.discount ? row(`折扣（${escapeHtml(o.coupon_code)}）`, '- ' + money(o.discount)) : '') +
    row('運費', '+ ' + money(o.shipping_fee)) +
    (o.donation ? row('加碼贊助', '+ ' + money(o.donation)) : '') +
    `<div class="line total"><span>總金額</span><span class="money">${money(o.total)}</span></div>`;

  const s = o.shipping;
  $('shipping').innerHTML = `${escapeHtml(s.recipient_name)}・${escapeHtml(s.recipient_phone)}<br>` +
    `${escapeHtml(s.zip)} ${escapeHtml(s.county)}${escapeHtml(s.district)}${escapeHtml(s.address)}<br>` +
    `<span class="muted">發票：${o.invoice.type === 'company' ? `公司 ${escapeHtml(o.invoice.tax_id)} ${escapeHtml(o.invoice.title)}` : `個人 ${escapeHtml(o.invoice.carrier || '寄送至 ' + o.customer.email)}`}</span>`;
}

function showError(msg) {
  $('error').textContent = msg;
  $('error').classList.remove('hidden');
}
