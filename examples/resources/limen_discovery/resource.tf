resource "limen_discovery" "aws_prod" {
  name = "aws-prod"

  spec = {
    provider        = "aws"
    accounts        = [{ role_arn = "arn:aws:iam::111122223333:role/limen-discovery" }]
    regions         = ["eu-west-1"]
    interval        = "5m"
    worker_selector = { pool = "prod-vpc" }
    match = [{
      type             = "rds"
      labels_from_tags = ["env", "team"]
    }]
  }
}
