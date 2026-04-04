resource "aws_iam_role" "lambda" {
  for_each = local.lambdas

  name               = "${local.name}-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume_role.json
}

resource "aws_iam_role_policy_attachment" "lambda_basic" {
  for_each = local.lambdas

  role       = aws_iam_role.lambda[each.key].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy" "ssm" {
  for_each = local.lambdas

  name   = "ssm"
  role   = aws_iam_role.lambda[each.key].name
  policy = data.aws_iam_policy_document.lambda_ssm.json
}

resource "aws_iam_role_policy" "receiver_ddb_write" {
  name   = "ddb"
  role   = aws_iam_role.lambda["receiver"].name
  policy = data.aws_iam_policy_document.lambda_receiver_ddb_write.json
}

resource "aws_iam_role_policy" "dispatcher_ddb_streams" {
  name   = "ddb_streams"
  role   = aws_iam_role.lambda["dispatcher"].name
  policy = data.aws_iam_policy_document.lambda_dispatcher_ddb_stream.json
}

