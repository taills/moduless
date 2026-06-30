// API client and auth-token helpers for the host app. The session token is kept
// in localStorage (for the host's own Authorization header) AND mirrored to a
// "moduless_token" cookie so qiankun sub-apps' same-origin /api/extensions calls
// authenticate automatically without threading the token through every request.
const TOKEN_KEY = "moduless_token";

export const auth = {
  get token() {
    return localStorage.getItem(TOKEN_KEY) || "";
  },
  setToken(value) {
    if (value) {
      localStorage.setItem(TOKEN_KEY, value);
      document.cookie = `${TOKEN_KEY}=${value}; path=/; SameSite=Lax`;
    } else {
      localStorage.removeItem(TOKEN_KEY);
      document.cookie = `${TOKEN_KEY}=; path=/; Max-Age=0; SameSite=Lax`;
    }
  },
};

export async function api(path, options = {}) {
  const res = await fetch("/api" + path, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      ...(auth.token ? { Authorization: "Bearer " + auth.token } : {}),
      ...(options.headers || {}),
    },
  });
  if (res.status === 401) {
    auth.setToken("");
    throw new Error("unauthenticated");
  }
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}
