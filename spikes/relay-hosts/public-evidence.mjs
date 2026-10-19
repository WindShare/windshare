const accountField = /^(account|account_id|accountId|accountTag)$/;
const accountId = /^[a-f0-9]{32}$/i;
const graphqlAccount = /(\baccountTag\s*:\s*")[a-f0-9]{32}(")/gi;
const workerHost = /\b([a-z0-9-]+)\.[a-z0-9-]+\.workers\.dev\b/gi;

// Publication must not carry account selectors from the locally retained captures.
export function formatPublicEvidence(value) {
  return JSON.stringify(value, (key, item) => {
    if (typeof item !== 'string') return item;
    if (accountField.test(key) && accountId.test(item)) return '<ACCOUNT_ID>';
    return item
      .replace(graphqlAccount, '$1<ACCOUNT_ID>$2')
      .replace(workerHost, '$1.workers.example.invalid');
  }, 2) + '\n';
}
