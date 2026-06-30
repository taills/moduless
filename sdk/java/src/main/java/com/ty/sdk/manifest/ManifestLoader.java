package com.ty.sdk.manifest;

import com.ty.sdk.proto.CollectionSchema;
import com.ty.sdk.proto.IndexSchema;
import com.ty.sdk.proto.RegisterRequest;
import com.ty.sdk.proto.SlotSchema;
import org.yaml.snakeyaml.Yaml;

import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.List;
import java.util.Map;

/**
 * Loads an extension manifest.yaml and applies its declarations to a
 * {@link RegisterRequest} so Core can provision CMDS tables/indexes and register
 * UI slots, matching the Go and Python SDKs.
 */
public final class ManifestLoader {

    private ManifestLoader() {}

    @SuppressWarnings("unchecked")
    public static void apply(String path, RegisterRequest.Builder req) throws IOException {
        Map<String, Object> manifest;
        try (InputStream in = new FileInputStream(path)) {
            manifest = new Yaml().load(in);
        }
        if (manifest == null) {
            return;
        }

        Object weight = manifest.get("weight");
        if (weight instanceof Number) {
            req.setWeight(((Number) weight).intValue());
        }

        Map<String, Object> database = (Map<String, Object>) manifest.get("database");
        if (database != null) {
            List<Map<String, Object>> collections = (List<Map<String, Object>>) database.get("collections");
            if (collections != null) {
                for (Map<String, Object> c : collections) {
                    CollectionSchema.Builder col = CollectionSchema.newBuilder()
                            .setName(String.valueOf(c.get("name")));
                    List<Map<String, Object>> indexes = (List<Map<String, Object>>) c.get("indexes");
                    if (indexes != null) {
                        for (Map<String, Object> idx : indexes) {
                            IndexSchema.Builder i = IndexSchema.newBuilder();
                            List<String> fields = (List<String>) idx.get("fields");
                            if (fields != null) {
                                i.addAllFields(fields);
                            }
                            i.setUnique(Boolean.TRUE.equals(idx.get("unique")));
                            col.addIndexes(i);
                        }
                    }
                    req.addCollections(col);
                }
            }
        }

        Object displayName = manifest.get("display_name");
        if (displayName != null) {
            req.setDisplayName(displayName.toString());
        }
        Map<String, Object> menu = (Map<String, Object>) manifest.get("menu");
        if (menu != null) {
            if (menu.get("icon") != null) {
                req.setMenuIcon(menu.get("icon").toString());
            }
            if (menu.get("path") != null) {
                req.setMenuPath(menu.get("path").toString());
            }
        }

        List<Map<String, Object>> slots = (List<Map<String, Object>>) manifest.get("ui_slots");
        if (slots != null) {
            for (Map<String, Object> s : slots) {
                req.addSlots(SlotSchema.newBuilder()
                        .setSlotName(String.valueOf(s.getOrDefault("slot_name", "")))
                        .setComponentEntry(String.valueOf(s.getOrDefault("component_entry", ""))));
            }
        }
    }
}
