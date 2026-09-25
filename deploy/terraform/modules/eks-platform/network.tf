# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# VPC across three AZs: public subnets (load balancers, NAT), private subnets
# (nodes), and isolated database subnets with no route to the internet.

data "aws_availability_zones" "available" {
  state = "available"
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 3)
}

# terraform-aws-modules/vpc: the de-facto standard VPC module (subnet tiers,
# NAT, flow logs, subnet tagging for the AWS Load Balancer Controller).
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.7.3"

  name = var.name
  cidr = var.vpc_cidr
  azs  = local.azs

  public_subnets   = [for i in range(3) : cidrsubnet(var.vpc_cidr, 6, i)]
  private_subnets  = [for i in range(3) : cidrsubnet(var.vpc_cidr, 3, i + 1)]
  database_subnets = [for i in range(3) : cidrsubnet(var.vpc_cidr, 8, i + 16)]

  create_database_subnet_group           = true
  create_database_subnet_route_table     = true
  create_database_internet_gateway_route = false
  create_database_nat_gateway_route      = false

  enable_nat_gateway     = true
  single_nat_gateway     = !var.nat_gateway_per_az
  one_nat_gateway_per_az = var.nat_gateway_per_az

  enable_dns_hostnames = true
  enable_dns_support   = true

  # The default security group allows all traffic between its members; strip
  # it so nothing can rely on it by accident.
  manage_default_security_group  = true
  default_security_group_ingress = []
  default_security_group_egress  = []

  enable_flow_log                                 = true
  create_flow_log_cloudwatch_log_group            = true
  create_flow_log_cloudwatch_iam_role             = true
  flow_log_cloudwatch_log_group_retention_in_days = var.log_retention_days
  flow_log_cloudwatch_log_group_kms_key_id        = aws_kms_key.data.arn
  flow_log_max_aggregation_interval               = 60

  public_subnet_tags = {
    "kubernetes.io/role/elb" = 1
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = 1
  }

  tags = local.tags
}

# S3 through a gateway endpoint: no NAT charges for artifact traffic, and the
# artifact credentials are only valid through this endpoint (see s3.tf).
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = module.vpc.vpc_id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = module.vpc.private_route_table_ids
  tags              = merge(local.tags, { Name = "${var.name}-s3" })
}
