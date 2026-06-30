package com.ty.sdk.bridge;

import jakarta.servlet.ReadListener;
import jakarta.servlet.ServletInputStream;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletRequestWrapper;

import java.io.ByteArrayInputStream;
import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.Enumeration;
import java.util.HashMap;
import java.util.Map;

/**
 * Wraps a tunnelled request as a standard {@link HttpServletRequest} so Spring's
 * DispatcherServlet can route it without any network socket involved.
 */
public class MockHttpServletRequest extends HttpServletRequestWrapper {
    private final byte[] body;
    private final String method;
    private final String path;
    private final String queryString;
    private final Map<String, String> headers;

    public MockHttpServletRequest(HttpServletRequest request, String method, String path,
                                  String queryString, Map<String, String> headers, byte[] body) {
        super(request);
        this.method = method;
        this.path = path;
        this.queryString = queryString;
        this.headers = new HashMap<>(headers);
        this.body = body == null ? new byte[0] : body;
    }

    @Override
    public String getMethod() {
        return method;
    }

    @Override
    public String getRequestURI() {
        return path;
    }

    @Override
    public StringBuffer getRequestURL() {
        return new StringBuffer("http://core").append(path);
    }

    @Override
    public String getServletPath() {
        return path;
    }

    @Override
    public String getPathInfo() {
        return path;
    }

    @Override
    public String getQueryString() {
        return queryString;
    }

    @Override
    public String getHeader(String name) {
        for (Map.Entry<String, String> e : headers.entrySet()) {
            if (e.getKey().equalsIgnoreCase(name)) {
                return e.getValue();
            }
        }
        return null;
    }

    @Override
    public Enumeration<String> getHeaderNames() {
        return Collections.enumeration(headers.keySet());
    }

    @Override
    public Enumeration<String> getHeaders(String name) {
        String v = getHeader(name);
        if (v == null) {
            return Collections.emptyEnumeration();
        }
        return Collections.enumeration(Collections.singletonList(v));
    }

    @Override
    public int getContentLength() {
        return body.length;
    }

    @Override
    public long getContentLengthLong() {
        return body.length;
    }

    @Override
    public ServletInputStream getInputStream() {
        final ByteArrayInputStream bais = new ByteArrayInputStream(body);
        return new ServletInputStream() {
            @Override
            public boolean isFinished() {
                return bais.available() == 0;
            }

            @Override
            public boolean isReady() {
                return true;
            }

            @Override
            public void setReadListener(ReadListener readListener) {
            }

            @Override
            public int read() {
                return bais.read();
            }
        };
    }

    @Override
    public BufferedReader getReader() {
        return new BufferedReader(new InputStreamReader(getInputStream(), StandardCharsets.UTF_8));
    }
}
