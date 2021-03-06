package main

import (
	"encoding/gob"
	"flag"
	"log"
	"math"
	"os"
	"time"

	torch "github.com/wangkuiyi/gotorch"
	F "github.com/wangkuiyi/gotorch/nn/functional"
	"github.com/wangkuiyi/gotorch/nn/initializer"
	"github.com/wangkuiyi/gotorch/vision/imageloader"
	"github.com/wangkuiyi/gotorch/vision/models"
	"github.com/wangkuiyi/gotorch/vision/transforms"
)

const logInterval = 10 // in iterations

var device torch.Device

func maxIntSlice(v []int64) int64 {
	if len(v) == 0 {
		panic("maxIntSlice expected a non-empty slice")
	}
	max := v[0]
	for _, e := range v {
		if e > max {
			max = e
		}
	}
	return max
}

func rangeI(n int64) []int64 {
	res := []int64{}
	if n <= 0 {
		return res
	}
	for i := int64(0); i < n; i++ {
		res = append(res, i)
	}
	return res
}

func adjustLearningRate(opt torch.Optimizer, epoch int, lr float64) {
	// set the learning rate to the the initialize learning rate decayed
	// by 10 for every 30 epochs.
	newLR := lr * math.Pow(0.1, math.Floor(float64(epoch)/30.0))
	log.Printf("Adjust learning rate, epoch: %d, lr: %f", epoch, newLR)
	opt.SetLR(newLR)
}

func accuracy(output, target torch.Tensor, topk []int64) []float32 {
	maxk := maxIntSlice(topk)
	target = target.Detach()
	output = output.Detach()

	mbSize := target.Shape()[0]
	_, pred := torch.TopK(output, maxk, 1, true, true)
	pred = pred.Transpose(0, 1)
	correct := pred.Eq(target.View(1, -1).ExpandAs(pred))

	res := []float32{}
	for _, k := range topk {
		kt := torch.NewTensor(rangeI(k)).CopyTo(device)
		correctK := correct.IndexSelect(0, kt).View(-1).CastTo(torch.Float).Sum(map[string]interface{}{"dim": 0, "keepDim": true})
		res = append(res, correctK.Item().(float32)*100/float32(mbSize))
	}
	return res
}

func imageNetLoader(fn string, vocab map[string]int, mbSize int, pinMemory bool) *imageloader.ImageLoader {
	trans := transforms.Compose(
		transforms.RandomResizedCrop(224),
		transforms.RandomHorizontalFlip(0.5),
		transforms.ToTensor(),
		transforms.Normalize([]float32{0.485, 0.456, 0.406}, []float32{0.229, 0.224, 0.225}))

	loader, e := imageloader.New(fn, vocab, trans, mbSize, pinMemory, "rgb")
	if e != nil {
		log.Fatal(e)
	}
	return loader
}

func trainOneMinibatch(image, target torch.Tensor, model *models.ResnetModule, opt torch.Optimizer) (float32, float32, float32) {
	output := model.Forward(image)
	loss := F.CrossEntropy(output, target, torch.Tensor{}, -100, "mean")
	acc := accuracy(output, target, []int64{1, 5})
	acc1 := acc[0]
	acc5 := acc[1]
	loss.Backward()
	opt.Step()
	return loss.Item().(float32), acc1, acc5
}

func test(model *models.ResnetModule, loader *imageloader.ImageLoader, epoch int) {
	model.Train(false)
	testLoss := float32(0)
	acc1 := float32(0)
	acc5 := float32(0)
	correct := int64(0)
	iters := 0
	for loader.Scan() {
		data, label := loader.Minibatch()
		data = data.To(device, data.Dtype())
		label = label.To(device, label.Dtype())
		output := model.Forward(data)
		acc := accuracy(output, label, []int64{1, 5})
		acc1 += acc[0]
		acc5 += acc[1]
		loss := F.CrossEntropy(output, label, torch.Tensor{}, -100, "mean")
		pred := output.Argmax(1)
		testLoss += loss.Item().(float32)
		correct += pred.Eq(label.View(pred.Shape()...)).Sum(map[string]interface{}{"dim": 0, "keepDim": false}).Item().(int64)
		iters++
	}
	log.Printf("Test Epoch: %d, average loss: %.4f acc1: %.4f acc5: %.4f \n",
		epoch, testLoss/float32(iters), acc1/float32(iters), acc5/float32(iters))
}

func train(trainFn, testFn, label, save string, epochs int, pinMemory bool) {
	// build label vocabulary
	var vocab map[string]int
	if label == "" {
		v, e := imageloader.BuildLabelVocabularyFromTgz(trainFn)
		if e != nil {
			log.Fatal(e)
		}
		vocab = v
	} else {
		vocab = loadLabel(label)
	}

	log.Print("building label vocabulary done.")
	model := models.Resnet50()
	model.To(device)
	model.Train(true)

	// As the baseline implementation https://arxiv.org/pdf/1512.03385.pdf.
	// The learning rate is 0.1, with the mini-batch size 256 (32 images per GPUs).
	// Some times, we can scale the mini-batch size to improve the CUDA utilization.
	// When the mini-batch size scaled to 128(256 * k) on a single CUDA device,
	// to keep consistent with the baseline, we multiply the learning rate by k also.
	mbSize := 128
	lr := 0.1 * float64(mbSize) / 256
	momentum := 0.9
	weightDecay := 1e-4
	optimizer := torch.SGD(lr, momentum, 0, weightDecay, false)
	optimizer.AddParameters(model.Parameters())
	log.Printf("mini-batch size: %d, initialize LR: %f, momentum: %f, weight decay: %f", mbSize, lr, momentum, weightDecay)
	for epoch := 0; epoch < epochs; epoch++ {
		adjustLearningRate(optimizer, epoch, lr)
		trainLoader := imageNetLoader(trainFn, vocab, mbSize, pinMemory)
		testLoader := imageNetLoader(testFn, vocab, mbSize, pinMemory)
		iter := 0
		startTime := time.Now()
		for trainLoader.Scan() {
			iter++
			data, label := trainLoader.Minibatch()
			optimizer.ZeroGrad()
			loss, acc1, acc5 := trainOneMinibatch(data.To(device, data.Dtype()), label.To(device, label.Dtype()), model, optimizer)
			if iter%logInterval == 0 {
				throughput := float64(data.Shape()[0]*logInterval) / time.Since(startTime).Seconds()
				log.Printf("Train Epoch: %d, Iteration: %d, loss:%f, acc1: %f, acc5:%f, throughput: %f samples/sec", epoch, iter, loss, acc1, acc5, throughput)
				startTime = time.Now()
			}
		}
		test(model, testLoader, epoch)
	}
	saveModel(model, save)
}

func loadLabel(labelFn string) map[string]int {
	f, e := os.Open(labelFn)
	if e != nil {
		log.Fatal(e)
	}
	defer f.Close()

	labels := make(map[string]int)
	if e := gob.NewDecoder(f).Decode(&labels); e != nil {
		log.Fatal(e)
	}
	return labels
}

func saveModel(model *models.ResnetModule, modelFn string) {
	log.Println("Saving model to", modelFn)
	f, e := os.Create(modelFn)
	if e != nil {
		log.Fatalf("Cannot create file to save model: %v", e)
	}
	defer f.Close()

	d := torch.NewDevice("cpu")
	model.To(d)
	if e := gob.NewEncoder(f).Encode(model.StateDict()); e != nil {
		log.Fatal(e)
	}
}

func main() {
	if torch.IsCUDAAvailable() {
		log.Println("CUDA is valid")
		device = torch.NewDevice("cuda")
	} else {
		log.Println("No CUDA found; CPU only")
		device = torch.NewDevice("cpu")
	}

	initializer.ManualSeed(1)
	trainTar := flag.String("data", "/tmp/imagenet_training_shuffled.tar.gz", "data tarball")
	testTar := flag.String("test", "/tmp/imagenet_testing_shuffled.tar.gz", "data tarball")
	label := flag.String("label", "", "label vocabulary")
	save := flag.String("save", "/tmp/imagenet_model.gob", "the model file")
	epochs := flag.Int("epochs", 5, "the number of epochs")
	pinMemory := flag.Bool("pin_memory", false, "use pinned memory")

	flag.Parse()

	train(*trainTar, *testTar, *label, *save, *epochs, *pinMemory && torch.IsCUDAAvailable())
}
