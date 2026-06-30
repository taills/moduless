package com.ty.sdk.context;

import java.util.Collections;
import java.util.List;

/**
 * Request-scoped authenticated user, propagated via ThreadLocal so business
 * controllers can read the identity Core injected over the tunnel.
 */
public class UserContext {
    private static final ThreadLocal<UserContext> CONTEXT = new ThreadLocal<>();

    private final String userId;
    private final List<String> roles;
    private final List<String> permissions;

    public UserContext(String userId, List<String> roles, List<String> permissions) {
        this.userId = userId;
        this.roles = roles == null ? Collections.emptyList() : roles;
        this.permissions = permissions == null ? Collections.emptyList() : permissions;
    }

    public static UserContext get() {
        return CONTEXT.get();
    }

    public static void set(UserContext uc) {
        CONTEXT.set(uc);
    }

    public static void clear() {
        CONTEXT.remove();
    }

    public String getUserId() {
        return userId;
    }

    public List<String> getRoles() {
        return roles;
    }

    public List<String> getPermissions() {
        return permissions;
    }
}
