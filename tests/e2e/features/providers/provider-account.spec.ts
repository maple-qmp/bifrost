import { expect, test } from "../../core/fixtures/base.fixture";

// All account operations are mocked: this suite never reads credentials or opens an OAuth grant.
test.describe("Local provider accounts", () => {
 test.use({ skipAutoLogin: true });
 for (const provider of ["chatgpt", "codebuddy"]) {
  test(`${provider} connects, reauthorizes, reports errors and disconnects`, async ({ page }) => {
   let connected = false;
   let loginState = "";
   let fail = false;
   const config = { name: provider, provider_status: "active", network_config: { base_url: "", default_request_timeout_in_seconds: 300, max_retries: 0, retry_backoff_initial: 500, retry_backoff_max: 5000 }, concurrency_and_buffer_size: { concurrency: 10, buffer_size: 10 }, keys: [] };
   await page.route("**/api/providers**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/account/login")) {
     loginState = "pending";
     await route.fulfill({ json: { session_id: "mock-session", url: "https://auth.openai.com/mock-authorization" } }); return;
    }
    if (path.endsWith("/account/detect") || path.endsWith("/account/callback")) {
     if (fail) { await route.fulfill({ status: 400, json: { is_bifrost_error: false, status_code: 400, error: { message: "Mock authorization failed" } } }); return; }
     if (path.endsWith("/callback")) expect(route.request().postDataJSON()).toEqual({ session_id: "mock-session", url: "http://127.0.0.1:1455/auth/callback?code=mock&state=mock" });
     connected = true; loginState = "completed";
     await route.fulfill({ json: { connected, identity: "mock-account", login_state: loginState } }); return;
    }
    if (path.endsWith("/account")) {
     if (route.request().method() === "DELETE") { connected = false; loginState = ""; }
     await route.fulfill({ json: { connected, identity: connected ? "mock-account" : undefined, login_state: loginState } }); return;
    }
    if (path.endsWith("/keys")) { await route.fulfill({ json: { keys: [], total: 0 } }); return; }
    await route.fulfill({ json: path === "/api/providers" ? { providers: [config], total: 1 } : config });
   });
   await page.goto(`/workspace/providers?provider=${provider}`);
   await expect(page.getByTestId("provider-account-panel")).toBeVisible();
   await expect(page.getByTestId("provider-account-status")).toContainText("Disconnected");
   const connect = async () => {
    await page.getByTestId("provider-account-connect").click();
    if (provider === "chatgpt") {
     await expect(page.getByTestId("provider-account-login-link")).toHaveAttribute("href", "https://auth.openai.com/mock-authorization");
     await page.getByTestId("provider-account-callback").fill("http://127.0.0.1:1455/auth/callback?code=mock&state=mock");
     await page.getByTestId("provider-account-complete").click();
    }
   };
   if(provider === "codebuddy") {
    await page.getByTestId("provider-account-credential-file").fill("/tmp/mock-codebuddy.info");
    await page.getByTestId("provider-account-prompt-compat").click();
    const saved = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/providers/codebuddy" && request.method() === "PUT");
    await page.getByTestId("provider-account-save").click();
    expect((await saved).postDataJSON().local_account_config).toEqual({credential_file:"/tmp/mock-codebuddy.info",prompt_compat:false});
   }
   await connect();
   await expect(page.getByTestId("provider-account-status")).toContainText("Connected");
   await connect();
   await expect(page.getByTestId("provider-account-status")).toContainText("Connected");
   fail = true;
   await connect();
   await expect(page.getByTestId("provider-account-error")).toContainText("Mock authorization failed");
   await page.getByTestId("provider-account-disconnect").click();
   await expect(page.getByTestId("provider-account-status")).toContainText("Disconnected");
  });
 }
});
