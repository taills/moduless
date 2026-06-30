package com.example.demo;

import com.ty.sdk.context.UserContext;
import com.ty.sdk.db.DbClient;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.DeleteMapping;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.PutMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;

/** REST CRUD over the CMDS "items" collection, served via Core's gRPC tunnel. */
@RestController
public class ItemController {

    private static final String COLLECTION = "items";

    private final DbClient db;

    public ItemController(DbClient db) {
        this.db = db;
    }

    @GetMapping("/info")
    public Map<String, Object> info() {
        UserContext user = UserContext.get();
        Map<String, Object> body = new HashMap<>();
        body.put("language", "java");
        body.put("user_id", user != null ? user.getUserId() : "anonymous");
        body.put("roles", user != null ? user.getRoles() : List.of());
        return body;
    }

    @GetMapping("/items")
    public Map<String, Object> list(
            @RequestParam(required = false, defaultValue = "") String status,
            @RequestParam(defaultValue = "100") int limit,
            @RequestParam(defaultValue = "0") int offset) throws Exception {
        List<Item> items = status.isEmpty()
                ? db.find(COLLECTION, limit, offset, Item.class)
                : db.find(COLLECTION, "status", "=", status, limit, offset, Item.class);
        Map<String, Object> body = new HashMap<>();
        body.put("items", items);
        body.put("count", items.size());
        return body;
    }

    @PostMapping("/items")
    public ResponseEntity<?> create(@RequestBody Item in) throws Exception {
        if (isBlank(in.name()) || isBlank(in.code())) {
            return ResponseEntity.badRequest().body(Map.of("error", "name and code are required"));
        }
        Item item = new Item(UUID.randomUUID().toString(), in.name(), in.code(), statusOrDefault(in.status()));
        db.put(COLLECTION, item.id(), item);
        return ResponseEntity.status(201).body(item);
    }

    @GetMapping("/items/{id}")
    public ResponseEntity<?> get(@PathVariable String id) throws Exception {
        Item item = db.get(COLLECTION, id, Item.class);
        if (item == null) {
            return ResponseEntity.status(404).body(Map.of("error", "not found"));
        }
        return ResponseEntity.ok(item);
    }

    @PutMapping("/items/{id}")
    public ResponseEntity<?> update(@PathVariable String id, @RequestBody Item in) throws Exception {
        if (isBlank(in.name()) || isBlank(in.code())) {
            return ResponseEntity.badRequest().body(Map.of("error", "name and code are required"));
        }
        Item item = new Item(id, in.name(), in.code(), statusOrDefault(in.status()));
        db.put(COLLECTION, id, item);
        return ResponseEntity.ok(item);
    }

    @DeleteMapping("/items/{id}")
    public Map<String, Object> delete(@PathVariable String id) {
        db.delete(COLLECTION, id);
        return Map.of("ok", true);
    }

    private static boolean isBlank(String s) {
        return s == null || s.isBlank();
    }

    private static String statusOrDefault(String s) {
        return isBlank(s) ? "active" : s;
    }
}
