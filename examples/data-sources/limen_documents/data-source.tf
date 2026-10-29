data "limen_documents" "staging_dbs" {
  kind   = "resource"
  labels = { team = "payments", env = "staging" }
}

output "staging_dbs" {
  value = data.limen_documents.staging_dbs.names
}
