package com.example.demo;

/**
 * Demo CRUD resource. Core provisions the "items" table and its indexes
 * (status, unique code) from manifest.yaml. Jackson maps this record to/from
 * the JSON documents stored in CMDS.
 */
public record Item(String id, String name, String code, String status) {}
