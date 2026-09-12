// Component contracts need a real origin without starting the application's receiver.
// Harnesses own their runtime and DOM; entry-point contracts navigate to the app explicitly.
export const BROWSER_CONTRACT_HOST_PATH = '/test/browser/contract-host.html'
