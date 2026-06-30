package com.ty.sdk.bridge;

import jakarta.servlet.ServletOutputStream;
import jakarta.servlet.WriteListener;
import jakarta.servlet.http.HttpServletResponse;
import jakarta.servlet.http.HttpServletResponseWrapper;

import java.io.ByteArrayOutputStream;
import java.io.PrintWriter;
import java.io.OutputStreamWriter;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Captures the servlet pipeline output (status, headers, body) so it can be
 * marshalled into a HttpResponseChunk and pushed back over the tunnel.
 */
public class MockHttpServletResponse extends HttpServletResponseWrapper {
    private int status = 200;
    private final Map<String, String> headers = new LinkedHashMap<>();
    private final ByteArrayOutputStream buffer = new ByteArrayOutputStream();
    private PrintWriter writer;

    public MockHttpServletResponse(HttpServletResponse response) {
        super(response);
    }

    public int getCapturedStatus() {
        return status;
    }

    public Map<String, String> getCapturedHeaders() {
        return headers;
    }

    public byte[] getCapturedBody() {
        if (writer != null) {
            writer.flush();
        }
        return buffer.toByteArray();
    }

    @Override
    public void setStatus(int sc) {
        this.status = sc;
    }

    @Override
    public int getStatus() {
        return status;
    }

    @Override
    public void setHeader(String name, String value) {
        headers.put(name, value);
    }

    @Override
    public void addHeader(String name, String value) {
        headers.put(name, value);
    }

    @Override
    public void setContentType(String type) {
        headers.put("Content-Type", type);
    }

    @Override
    public ServletOutputStream getOutputStream() {
        return new ServletOutputStream() {
            @Override
            public boolean isReady() {
                return true;
            }

            @Override
            public void setWriteListener(WriteListener writeListener) {
            }

            @Override
            public void write(int b) {
                buffer.write(b);
            }
        };
    }

    @Override
    public PrintWriter getWriter() {
        if (writer == null) {
            writer = new PrintWriter(new OutputStreamWriter(buffer, StandardCharsets.UTF_8), false);
        }
        return writer;
    }
}
