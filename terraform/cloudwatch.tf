resource "aws_cloudwatch_log_group" "lambda" {
  for_each = local.lambdas

  name              = "/aws/lambda/${local.name}-${each.key}"
  retention_in_days = 1
}

resource "aws_cloudwatch_event_rule" "scheduler" {
  for_each = local.schedules

  name                = "${local.name}-${each.key}"
  description         = each.value.description
  schedule_expression = each.value.schedule_expression
}

resource "aws_cloudwatch_event_target" "scheduler" {
  for_each = local.schedules

  rule = aws_cloudwatch_event_rule.scheduler[each.key].name
  arn  = aws_lambda_function.lambda["scheduler"].arn
}

