package io.treeverse.clients

import com.amazonaws.services.s3.{AmazonS3, model}
import com.azure.core.http.rest.Response
import com.azure.storage.blob.batch.{BlobBatchClient, BlobBatchClientBuilder}
import com.azure.storage.blob.models.DeleteSnapshotsOptionType
import com.azure.storage.blob.{BlobServiceClient, BlobServiceClientBuilder}
import com.azure.storage.common.StorageSharedKeyCredential
import org.apache.hadoop.conf.Configuration

import java.net.URI
import collection.JavaConverters._

trait BulkRemover {
  def constructRemoveKeyNames( keys: Seq[String], snPrefix: String): Seq[String] = {
    if (keys.isEmpty) return Seq.empty
    keys.map(x => snPrefix.concat(x))
  }

  def deleteObjects(keys: Seq[String], snPrefix: String) : Seq[String]
}

object BulkRemoverFactory {
  val StorageTypeS3 = "s3"
  val StorageTypeAzure = "azure"

  private class S3BulkRemover(hc: Configuration, storageNamespace: String, region: String, numRetries: Int) extends BulkRemover {
    val uri = new URI(storageNamespace)
    val bucket = uri.getHost
    val s3Client = getS3Client(hc, bucket, region, numRetries)

    override def deleteObjects(keys: Seq[String], snPrefix: String): Seq[String] = {
      val removeKeyNames = constructRemoveKeyNames(keys, snPrefix)
      println("Remove keys:", removeKeyNames.take(100).mkString(", "))

      val removeKeys = removeKeyNames.map(k => new model.DeleteObjectsRequest.KeyVersion(k)).asJava

      val delObjReq = new model.DeleteObjectsRequest(bucket).withKeys(removeKeys)
      val res = s3Client.deleteObjects(delObjReq)
      res.getDeletedObjects.asScala.map(_.getKey())
    }

    private def getS3Client(
                             hc: Configuration,
                             bucket: String,
                             region: String,
                             numRetries: Int
                           ): AmazonS3 =
      io.treeverse.clients.conditional.S3ClientBuilder.build(hc, bucket, region, numRetries)
  }

  //TODO: can I use numRetries in Azure batch client at all?
  private class AzureBlobBulkRemover(hc: Configuration, storageNamespace: String, numRetries: Int) extends BulkRemover {
    val StorageAccountKeyPropertyPattern = "fs.azure.account.key.<storageAccountName>.dfs.core.windows.net"
    val uri = new URI(storageNamespace)
    // example storage namespace https://<storageAccountName>.blob.core.windows.net/talscontainer/repo6/
    val storageAccountUrl = uri.getHost // storage account url = https://<storageAccountName>.blob.core.windows.net
    val storageAccountName = storageAccountUrl.split('.')(0)

    val blobBatchClient = getBlobBatchClient(hc, storageAccountUrl, storageAccountName)

    override def deleteObjects(keys: Seq[String], snPrefix: String): Seq[String] = {
      val removeKeyNames = constructRemoveKeyNames(keys, snPrefix)
      println("Remove keys:", removeKeyNames.take(100).mkString(", "))

      val removeKeys = removeKeyNames.asJava

      blobBatchClient.deleteBlobs(removeKeys, DeleteSnapshotsOptionType.INCLUDE)
        .forEach((response: Response[Void]) => System.out.println("Deleting blob with URL %s completed with status code %d%n",
          response.getRequest.getUrl, response.getStatusCode))

      // TODO: change this
      Seq.empty
    }

    private def getBlobBatchClient(hc: Configuration, storageAccountUrl: String, storageAccountName: String): BlobBatchClient = {
      // Storage account keys are values of the spark propery with name spark.hadoop.fs.azure.account.key.storageAccountName.dfs.core.windows.net
      val storageAccountKeyPropName = StorageAccountKeyPropertyPattern.replaceFirst("<storageAccountName>", storageAccountName)
      val storageAccountKey = hc.get(storageAccountKeyPropName)
      val blobServiceClientSharedKey: BlobServiceClient =
        new BlobServiceClientBuilder().endpoint(storageAccountUrl)
          .credential(new StorageSharedKeyCredential(storageAccountName, storageAccountKey)).buildClient

      new BlobBatchClientBuilder(blobServiceClientSharedKey).buildClient
    }

  }

  def apply(storageType: String, hc: Configuration, storageNamespace: String, numRetries: Int, region: String): BulkRemover = {
    if (storageType == StorageTypeS3) {
      new S3BulkRemover(hc, storageNamespace, region, numRetries)
    } else if (storageType == StorageTypeAzure) {
      new AzureBlobBulkRemover(hc, storageNamespace, numRetries)
    } else {
      throw new IllegalArgumentException("Invalid argument.")
    }
  }
}
